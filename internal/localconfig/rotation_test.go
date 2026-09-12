package localconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExactReferencesAndStalePreview(t *testing.T) {
	old := "tmx_" + strings.Repeat("a", 43)
	next := "tmx_" + strings.Repeat("b", 43)
	for _, ext := range []string{"yaml", "json", "toml", "env"} {
		t.Run(ext, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config."+ext)
			before := "# retained comment\r\nkey=\"" + old + "\"\r\nprovider_key=\"keep-this\"\r\n"
			if err := os.WriteFile(p, []byte(before), 0600); err != nil {
				t.Fatal(err)
			}
			refs, err := Inspect(context.Background(), []string{p}, func(_ context.Context, value string) (bool, error) { return value == old, nil })
			if err != nil || len(refs) != 1 || refs[0].Count != 1 {
				t.Fatal(refs, err)
			}
			if err = Apply(refs, next); err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(p)
			if string(data) != strings.ReplaceAll(before, old, next) {
				t.Fatal("unrelated bytes changed")
			}
			if err = Apply(refs, next); err == nil {
				t.Fatal("stale snapshot accepted")
			}
		})
	}
}
