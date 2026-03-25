package framework

import (
	"os"
	"time"
)

const (
	shortTimeoutEnv  = "E2E_SHORT_TIMEOUT"
	middleTimeoutEnv = "E2E_MIDDLE_TIMEOUT"
	longTimeoutEnv   = "E2E_LONG_TIMEOUT"
	maxTimeoutEnv    = "E2E_MAX_TIMEOUT"
)

var (
	ShortTimeout    = getTimeout(shortTimeoutEnv, 30*time.Second)
	MiddleTimeout   = getTimeout(middleTimeoutEnv, 60*time.Second)
	LongTimeout     = getTimeout(longTimeoutEnv, 300*time.Second)
	MaxTimeout      = getTimeout(maxTimeoutEnv, 600*time.Second)
	PollingInterval = 1 * time.Second
)

func getTimeout(env string, defaultTimeout time.Duration) time.Duration {
	if e, ok := os.LookupEnv(env); ok {
		t, err := time.ParseDuration(e)
		if err != nil {
			return defaultTimeout
		}
		return t
	}
	return defaultTimeout
}
