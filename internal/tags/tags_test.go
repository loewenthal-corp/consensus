package tags

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeList(t *testing.T) {
	require.Equal(t, []string{
		"postgres",
		"repo:github.com/Org/Repo",
		"tool:turbo",
	}, NormalizeList([]string{" Postgres ", "tool=turbo", "repo:github.com/Org/Repo", "postgres"}))
}

func TestParse(t *testing.T) {
	require.Equal(t, []string{
		"build",
		"source-maps",
		"tool:turbo",
	}, Parse("build, source-maps tool=turbo"))
}
