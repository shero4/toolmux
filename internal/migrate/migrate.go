package migrate

import (
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var files embed.FS

type Migration struct {
	Version int
	SQL     string
}

func All() ([]Migration, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	var migrations []Migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %q has no numeric prefix", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", entry.Name(), err)
		}
		data, err := files.ReadFile(entry.Name())
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, Migration{Version: version, SQL: string(data)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}
