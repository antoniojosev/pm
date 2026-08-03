package runner

import (
	"fmt"
	"path/filepath"

	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/scan"
)

// AdhocOpts configure `pm run`.
type AdhocOpts struct {
	LaunchOpts
	Name   string // override derived name
	Always bool   // register + launch as persistent service tier
}

// RunAdhoc implements `pm run -- <cmd>`: it resolves the project from dir
// (registering a fresh entry if unknown), records the command, and launches.
func (r *Runner) RunAdhoc(dir string, cmd []string, opts AdhocOpts) (Result, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, err
	}

	p, existed := r.Store.FindByDir(abs)
	if !existed {
		name := opts.Name
		if name == "" {
			name = filepath.Base(abs)
		}
		det, _ := scan.Detect(abs)
		p = registry.Project{
			Name:         name,
			Dir:          abs,
			Kind:         det.Kind,
			Tier:         registry.TierEphemeral,
			PreferPort:   det.PreferPort,
			PortStrategy: det.PortStrategy,
			Stack:        det.Stack,
		}
	}
	if opts.Name != "" {
		p.Name = opts.Name
	}
	if p.Kind == "" {
		p.Kind = registry.KindNative // unknown stack still runs & collides as native
	}
	if len(cmd) > 0 {
		p.Cmd = cmd // explicit command always wins and is remembered
	}
	if len(p.Cmd) == 0 {
		return Result{}, fmt.Errorf("no command: use `pm run -- <cmd>` or register the project")
	}
	p.Draft = false
	if opts.Always {
		p.Tier = registry.TierService
	}
	if err := r.Store.Upsert(p); err != nil {
		return Result{}, err
	}
	return r.launch(p, opts.LaunchOpts)
}
