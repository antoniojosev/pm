package api

import (
	"fmt"

	"github.com/antoniojosev/pm/internal/registry"
	"github.com/antoniojosev/pm/internal/runner"
)

// StartRun launches the process a RunSpec describes without registering it
// (POST /api/v1/run, `pm run --name x -- cmd`, the pm_run tool). It returns the
// unit, its pid and the ports the run holds.
//
// A declared port must have no listener (the `ss` snapshot) and must pass the
// test bind (see ports.Free), so a port `ss` cannot see still counts as taken.
// Leases and other runs are checked atomically with the write that records the
// run, in the registry: a port leased to the run's own owner is the intended
// case, one leased to someone else is refused.
func (s *Service) StartRun(spec runner.RunSpec) (runner.RunResult, error) {
	spec, err := spec.Normalize()
	if err != nil {
		return runner.RunResult{}, err
	}
	// Name first: when it is taken, that is the error to report, not the port
	// its running owner holds. The registry re-checks it atomically.
	_ = s.Store.Reload()
	if existing, ok := s.Store.Get(spec.Name); ok {
		if err := registry.NameTaken(existing, existing.IsRun() && !s.Run.IsActive(existing.Name)); err != nil {
			return runner.RunResult{}, err
		}
	}
	if len(spec.Ports) > 0 {
		live := s.Run.LivePorts()
		for _, p := range spec.Ports {
			if live[p] {
				return runner.RunResult{}, fmt.Errorf("port %d is in use: something is already listening on it", p)
			}
			if !s.PortFree(p) {
				return runner.RunResult{}, fmt.Errorf("port %d is not free: it cannot be bound", p)
			}
		}
	}
	return s.Run.LaunchSpec(spec)
}
