package tools

import "regexp"

var conventionalEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// EnvNameForDisplay is how an api_key_env value may appear in a message, a
// log or a tool result. The setting holds the name of a variable, but a key
// pasted there by mistake is an easy error to make, and these messages reach
// stderr, host logs kept on disk and — from a tool result — the model and the
// saved session. A conventional name (upper case, digits, underscores) is
// shown; anything else is described, never repeated.
func EnvNameForDisplay(name string) string {
	if conventionalEnvName.MatchString(name) {
		return name
	}
	return "api_key_env (it must be the name of an environment variable such as GOOGLE_PSE_API_KEY; what is set there does not look like one and is not shown, in case it is the key itself)"
}
