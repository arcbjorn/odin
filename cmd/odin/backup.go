package main

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/arcbjorn/odin/profile"
	"github.com/arcbjorn/odin/tools"
)

// cmdBackup writes a consistent snapshot of the profile database.
//
// A one-shot command, driven externally by a cron line or systemd timer —
// deliberately outside the agent process, the same operational pattern as the
// watchdog. It uses VACUUM INTO, so it is safe to run while the agent writes.
func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	common := bindCommon(fs)
	keep := fs.Int("keep", 7, "number of backups to retain")
	dir := fs.String("dir", "", "backup directory (default <profile>/backups)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}

	p, err := profile.Load(common.root, common.profile)
	if err != nil {
		return err
	}

	backupDir := *dir
	if backupDir == "" {
		backupDir = filepath.Join(p.Dir, "backups")
	}

	path, err := tools.Backup(p.DBPath, backupDir, *keep)
	if err != nil {
		return err
	}
	fmt.Printf("backup written to %s\n", path)
	return nil
}

// cmdRestore replaces the profile database with a backup.
func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	common := bindCommon(fs)
	from := fs.String("from", "", "backup file to restore, or \"latest\"")
	force := fs.Bool("force", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := common.resolve(); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("--from is required (a backup file, or \"latest\")")
	}

	p, err := profile.Load(common.root, common.profile)
	if err != nil {
		return err
	}

	source := *from
	if source == "latest" {
		backups, err := tools.ListBackups(filepath.Join(p.Dir, "backups"))
		if err != nil {
			return err
		}
		if len(backups) == 0 {
			return errors.New("no backups found")
		}
		source = backups[0]
	}

	// Restoring under a running agent corrupts both files. We cannot reliably
	// detect a running process, so require an explicit acknowledgement instead.
	if !*force {
		fmt.Printf("Restore %s over %s?\nStop the agent first — restoring under a running writer corrupts the database.\nType 'yes' to proceed: ", source, p.DBPath)
		var answer string
		fmt.Scanln(&answer)
		if answer != "yes" {
			return errors.New("restore cancelled")
		}
	}

	if err := tools.Restore(source, p.DBPath); err != nil {
		return err
	}
	fmt.Printf("restored %s -> %s\n", source, p.DBPath)
	return nil
}
