package server

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain guards the whole server integration suite against goroutine
// leaks (SPEC §18 资源回归).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
