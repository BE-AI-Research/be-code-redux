//go:build windows

package live

func syscallUmask(int) int { return 0 }
