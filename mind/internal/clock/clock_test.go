package clock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRealClock(t *testing.T) {
	got := RealClock{}.Now()
	assert.WithinDuration(t, time.Now(), got, 5*time.Second)
}

func TestRealIDGen(t *testing.T) {
	gen := RealIDGen{}
	a, b := gen.NewID("task"), gen.NewID("task")
	assert.Regexp(t, `^task-[0-9a-f]{16}$`, a)
	assert.NotEqual(t, a, b, "ids must be unique")
	bare := gen.NewID("")
	assert.Len(t, bare, 16)
	assert.NotContains(t, bare, "-")
}
