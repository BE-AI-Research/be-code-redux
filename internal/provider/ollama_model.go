package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Ollama's native management endpoints (/api/tags, /api/ps, /api/show,
// /api/pull, /api/generate) live here so ollama.go stays a chat file.

// ModelDetail is one row worth reading in a model list: what it costs to
// load, what it was quantized to, and whether it is already resident.
type ModelDetail struct {
	ID           string
	SizeBytes    int64
	Family       string
	Quantization string
	Window       int // 0 when unknown
	Resident     bool
}

// Describe is the one-line summary both model pickers show under a name, so
// the TUI's list and plain mode's /models never drift apart.
func (d ModelDetail) Describe() string {
	window := "ctx unknown"
	switch {
	case d.Window >= 1024:
		window = fmt.Sprintf("%dk ctx", (d.Window+512)/1024)
	case d.Window > 0:
		window = fmt.Sprintf("%d ctx", d.Window)
	}
	resident := ""
	if d.Resident {
		resident = " · loaded"
	}
	// A backend that reports no size reports no family or quantization
	// either (an OpenAI-compatible endpoint lists names and nothing else),
	// so the row is the window alone rather than "0.0GB  ·".
	if d.SizeBytes <= 0 {
		return window + resident
	}
	return strings.TrimSpace(fmt.Sprintf("%.1fGB %s %s",
		float64(d.SizeBytes)/1e9, d.Family, d.Quantization)) + " · " + window + resident
}

// ModelDetails asks p for its model rows, falling back to ListModels for a
// backend that has no richer listing (an OpenAI-compatible endpoint knows
// nothing about residency).
func ModelDetails(ctx context.Context, p Provider) ([]ModelDetail, error) {
	if d, ok := p.(ModelDetailer); ok {
		return d.Details(ctx)
	}
	models, err := p.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelDetail, 0, len(models))
	for _, m := range models {
		out = append(out, ModelDetail{ID: m.ID, SizeBytes: m.SizeBytes,
			Family: m.Family, Quantization: m.Quantization})
	}
	return out, nil
}

// psModel is one entry of /api/ps: a model currently held in memory, with
// the context window it was actually loaded with.
type psModel struct {
	Name          string `json:"name"`
	Model         string `json:"model"`
	ContextLength int    `json:"context_length"`
}

// running reads /api/ps. A decode failure is reported as an error; callers
// that can carry on without residency information ignore it.
func (p *Ollama) running(ctx context.Context) ([]psModel, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.APIBase+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Models []psModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Models, nil
}

// ListModels uses the native endpoint to get size/family/quant detail.
func (p *Ollama) ListModels(ctx context.Context) ([]ModelInfo, error) {
	tags, err := p.tags(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(tags))
	for _, m := range tags {
		out = append(out, ModelInfo{
			ID: m.Name, SizeBytes: m.Size,
			Family: m.Details.Family, Quantization: m.Details.QuantizationLevel,
		})
	}
	return out, nil
}

type tagModel struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Details struct {
		Family            string `json:"family"`
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
}

func (p *Ollama) tags(ctx context.Context) ([]tagModel, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.APIBase+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: HTTP %d listing models", resp.StatusCode)
	}
	var body struct {
		Models []tagModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Models, nil
}

// Details merges /api/tags with /api/ps so a model list can say what is on
// disk and what is already loaded (with the window it was loaded at). A
// failing /api/ps is not fatal: residency is simply unknown.
func (p *Ollama) Details(ctx context.Context) ([]ModelDetail, error) {
	tags, err := p.tags(ctx)
	if err != nil {
		return nil, err
	}
	live := map[string]psModel{}
	if ps, err := p.running(ctx); err == nil {
		for _, m := range ps {
			if m.Name != "" {
				live[m.Name] = m
			}
			if m.Model != "" {
				live[m.Model] = m
			}
		}
	}
	out := make([]ModelDetail, 0, len(tags))
	for _, m := range tags {
		d := ModelDetail{
			ID: m.Name, SizeBytes: m.Size,
			Family: m.Details.Family, Quantization: m.Details.QuantizationLevel,
		}
		if ps, ok := live[m.Name]; ok {
			d.Resident = true
			d.Window = ps.ContextLength
		}
		out = append(out, d)
	}
	return out, nil
}

// Pull downloads a model, reporting progress lines via onProgress.
func (p *Ollama) Pull(ctx context.Context, model string, onProgress func(string)) error {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: HTTP %d pulling %s", resp.StatusCode, model)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	last := ""
	for sc.Scan() {
		var ev struct {
			Status    string `json:"status"`
			Error     string `json:"error"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("ollama: %s", ev.Error)
		}
		line := ev.Status
		if ev.Total > 0 {
			line = fmt.Sprintf("%s %d%%", ev.Status, ev.Completed*100/ev.Total)
		}
		if line != last && onProgress != nil {
			onProgress(line)
			last = line
		}
	}
	return sc.Err()
}

// Running lists models currently loaded in memory.
func (p *Ollama) Running(ctx context.Context) ([]string, error) {
	models, err := p.running(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.Name)
	}
	return out, nil
}

// Resident reports whether the model is held in memory right now and, if so,
// the window it was loaded with. It reads /api/ps and nothing else: a caller
// that already knows the window it wants must be able to ask who is holding
// the model without the Modelfile probe — or, worse, a load — that answering
// "what window should I use?" can cost.
func (p *Ollama) Resident(ctx context.Context, model string) (int, bool, error) {
	ps, err := p.running(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, m := range ps {
		if m.Name == model || m.Model == model {
			return m.ContextLength, true, nil
		}
	}
	return 0, false, nil
}

// Status reports the live window and residency of model: loaded=true with
// the /api/ps window when resident, else loaded=false with the Modelfile
// num_ctx (0 if unknown).
func (p *Ollama) Status(ctx context.Context, model string) (int, bool, error) {
	if n, resident, err := p.Resident(ctx, model); err == nil && resident {
		return n, true, nil
	}
	n, err := p.ContextLength(ctx, model)
	return n, false, err
}

// KeepAlive extends the model's residency (see Warm).
func (p *Ollama) KeepAlive(ctx context.Context, model string, d time.Duration) error {
	return p.Warm(ctx, model, d)
}

// ContextLength reports the context window the server will actually use for
// model: the live value from /api/ps when loaded, else a Modelfile num_ctx
// from /api/show. 0 means unknown (Ollama's own default applies).
func (p *Ollama) ContextLength(ctx context.Context, model string) (int, error) {
	ps, err := p.running(ctx)
	if err == nil {
		for _, m := range ps {
			if (m.Name == model || m.Model == model) && m.ContextLength > 0 {
				return m.ContextLength, nil
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"name": model, "model": model})
	req, err := http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/show", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var show struct {
		Parameters string `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return 0, nil
	}
	for _, line := range strings.Split(show.Parameters, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "num_ctx" {
			if n, err := strconv.Atoi(f[1]); err == nil {
				return n, nil
			}
		}
	}
	return 0, nil
}

// Warm loads a model into memory with an extended keep-alive so the first
// real request doesn't pay the load cost.
func (p *Ollama) Warm(ctx context.Context, model string, keepAlive time.Duration) error {
	body := map[string]any{"model": model, "keep_alive": keepAlive.String()}
	// With the resolved options, because this call loads the model when it
	// is not resident (an eviction between requests is exactly when the
	// keep-alive refresh matters). Without them it would load at the
	// server's default window — outside the loader, which spec §10.1 says
	// is the only path that loads a model or sends num_ctx.
	if opts := p.loadOptions(); opts != nil {
		body["options"] = opts
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", p.APIBase+"/api/generate", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: HTTP %d warming %s", resp.StatusCode, model)
	}
	return nil
}

func (p *Ollama) Ping(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	models, err := p.ListModels(ctx)
	if err != nil {
		return "", err
	}
	running, _ := p.Running(ctx)
	return fmt.Sprintf("ok (%d models, %d loaded)", len(models), len(running)), nil
}
