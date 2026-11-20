package rules

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	"github.com/torrefatto/holism/mind/internal/planner"
)

func TestPlanSingleTaskFromGoal(t *testing.T) {
	p := &holismv1.Project{Id: "p1", Name: "web shop", Goal: "sell things"}
	specs, err := New().Plan(context.Background(), p, "Write the checkout flow\nimplement payment, add tests")
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Equal(t, "Write the checkout flow", specs[0].Title)
	assert.Equal(t, "Write the checkout flow\nimplement payment, add tests", specs[0].Description)
}

func TestPlanTruncatesLongTitle(t *testing.T) {
	p := &holismv1.Project{Id: "p1", Name: "p"}
	long := strings.Repeat("a", 200) + "\nbody"
	specs, err := New().Plan(context.Background(), p, long)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.Len(t, []rune(specs[0].Title), maxTitleLen, "title should be truncated to maxTitleLen runes")
	assert.True(t, strings.HasSuffix(specs[0].Title, "…"), "truncated title should end with …: %q", specs[0].Title)
}

func TestPlanErrors(t *testing.T) {
	p := &holismv1.Project{Id: "p1", Name: "p"}
	_, err := New().Plan(context.Background(), nil, "goal")
	assert.ErrorIs(t, err, planner.ErrNilProject)
	_, err = New().Plan(context.Background(), p, "")
	assert.ErrorIs(t, err, planner.ErrEmptyGoal)
	_, err = New().Plan(context.Background(), p, "  \n\t ")
	assert.ErrorIs(t, err, planner.ErrEmptyGoal)
}
