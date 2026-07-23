package profile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/arcbjorn/odin/jobs"
	"github.com/arcbjorn/odin/tools"
)

// ValidationReport summarizes an offline profile validation.
type ValidationReport struct {
	Profile     *Profile
	SystemFiles int
	Skills      int
	Jobs        int
	Migrations  int
	Timezone    string
	TimeSource  string
}

// Validate loads every declarative part of a profile without constructing a
// provider, reading credentials, applying migrations, or making network calls.
func Validate(root, name string) (*ValidationReport, error) {
	p, err := Load(root, name)
	if err != nil {
		return nil, err
	}
	zone, source, err := p.Timezone()
	if err != nil {
		return nil, err
	}
	if _, err := p.Location(); err != nil {
		return nil, err
	}

	migrations, err := LoadMigrations(p.MigrationsDir)
	if err != nil {
		return nil, err
	}
	report := &ValidationReport{
		Profile: p, SystemFiles: len(p.Config.SystemFiles),
		Migrations: len(migrations), Timezone: zone, TimeSource: source,
	}
	if p.HasToolset("db") {
		dsn := (&url.URL{Scheme: "file", Path: p.DBPath}).String() + "?mode=ro"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, fmt.Errorf("open database for validation: %w", err)
		}
		defer db.Close()
		if err := db.Ping(); err != nil {
			return nil, fmt.Errorf("validate database: %w", err)
		}
		if err := CheckMigrations(context.Background(), db, migrations, p.MigrationsDir); err != nil {
			return nil, err
		}
	}

	var skills *tools.Skills
	if p.HasToolset("skills") {
		skills, err = tools.NewSkills(p.SkillsDir)
		if err != nil {
			return nil, err
		}
		names, err := skills.List()
		if err != nil {
			return nil, err
		}
		report.Skills = len(names)
	}

	set, err := jobs.Load(p.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return report, nil
		}
		return nil, err
	}
	report.Jobs = len(set.Jobs)
	for _, job := range set.Jobs {
		if len(job.Skills) > 0 && skills == nil {
			return nil, fmt.Errorf("job %q declares skills but the skills toolset is disabled", job.Name)
		}
		seen := make(map[string]bool, len(job.Skills))
		for _, name := range job.Skills {
			if seen[name] {
				return nil, fmt.Errorf("job %q declares skill %q more than once", job.Name, name)
			}
			seen[name] = true
			if _, err := skills.Read(name); err != nil {
				return nil, fmt.Errorf("job %q: %w", job.Name, err)
			}
		}
	}
	return report, nil
}
