package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/runtime"
	"github.com/cubeplexai/cubepod/pkg/volume"
)

func (s *Server) plannedToPodBoxMounts(planned []volume.PlannedMount) []v1alpha1.E2BVolumeMount {
	if len(planned) == 0 {
		return nil
	}
	sharedPVC := ""
	if s != nil && s.livePodRouting && s.runtime == nil {
		sharedPVC = s.sharedE2BPVC()
	}
	out := make([]v1alpha1.E2BVolumeMount, 0, len(planned))
	for _, p := range planned {
		pvc, ok := controller.E2BPVCName(p.VolumeID)
		if !ok {
			continue
		}
		if sharedPVC != "" {
			out = append(out, v1alpha1.E2BVolumeMount{
				VolumeID: p.VolumeID,
				PVCName:  sharedPVC,
				SubPath:  p.VolumeID + "/data",
				Path:     p.Path,
			})
			continue
		}
		out = append(out, v1alpha1.E2BVolumeMount{
			VolumeID: p.VolumeID,
			PVCName:  pvc,
			Path:     p.Path,
		})
	}
	return out
}

func (s *Server) podBoxE2BMounts(sandboxID string) []v1alpha1.E2BVolumeMount {
	st := s.e2b.Load()
	if st == nil || sandboxID == "" {
		return nil
	}
	specs := st.MountsForSandbox(sandboxID)
	if len(specs) == 0 {
		return nil
	}
	planned := make([]volume.PlannedMount, 0, len(specs))
	for _, m := range specs {
		planned = append(planned, volume.PlannedMount{VolumeID: m.VolumeID, Name: m.VolumeName, Path: m.ContainerPath})
	}
	return s.plannedToPodBoxMounts(planned)
}

func (s *Server) withOfficialMounts(spec v1alpha1.PodBoxSpec, sandboxID string) v1alpha1.PodBoxSpec {
	if mounts := s.podBoxE2BMounts(sandboxID); len(mounts) > 0 {
		spec.E2BVolumeMounts = mounts
	}
	return spec
}

func (s *Server) runtimeVolumeMounts(sandboxID string) []runtime.SandboxVolumeMount {
	specs := s.E2BVolumeMounts(sandboxID)
	if len(specs) == 0 {
		return nil
	}
	out := make([]runtime.SandboxVolumeMount, 0, len(specs))
	for _, m := range specs {
		out = append(out, runtime.SandboxVolumeMount{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			DockerVolume:  m.DockerVolume,
		})
	}
	return out
}

func (s *Server) rollbackCreatedSandbox(ctx context.Context, sandboxID string) {
	if sandboxID == "" {
		return
	}
	if s.runtime != nil {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := s.runtime.Delete(dctx, sandboxID); err != nil {
			slog.Warn("runtime delete during rollback failed", "sandboxID", sandboxID, "err", err)
		}
		cancel()
	}
	if s.reconciler != nil {
		_ = s.reconciler.DeleteBox(ctx, sandboxID, false)
	}
	s.releaseSandboxVolumeMounts(sandboxID)
	s.releaseLocalSandbox(sandboxID)
}

func (s *Server) startRuntimeSandbox(ctx context.Context, rec *SandboxRecord, image string, env map[string]string) error {
	if s.runtime == nil || rec == nil {
		return nil
	}
	spec := runtime.SandboxSpec{
		SandboxID:        rec.SandboxID,
		TemplateID:       rec.TemplateID,
		VolumeKey:        rec.VolumeKey,
		RootfsGeneration: rec.RootfsGeneration,
		Image:            image,
		Env:              env,
		VolumeMounts:     s.runtimeVolumeMounts(rec.SandboxID),
	}
	info, err := s.runtime.EnsureRunning(ctx, spec)
	if err != nil {
		return fmt.Errorf("start sandbox runtime: %w", err)
	}
	if info != nil && info.PoddURL != "" {
		rec.PoddURL = info.PoddURL
		timeout := 30 * time.Second
		if probe := s.ProbeTimeout(); probe > 0 && probe < timeout {
			timeout = probe
		}
		_ = waitForPoddReady(ctx, rec.PoddURL, timeout)
	}
	return nil
}

func (s *Server) pauseRuntimeSandbox(ctx context.Context, sandboxID string) {
	if s.runtime == nil || sandboxID == "" {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.runtime.Pause(dctx, sandboxID); err != nil {
		slog.Warn("runtime pause failed", "sandboxID", sandboxID, "err", err)
	}
}

func (s *Server) deleteRuntimeSandbox(ctx context.Context, sandboxID string) {
	if s.runtime == nil || sandboxID == "" {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.runtime.Delete(dctx, sandboxID); err != nil {
		slog.Warn("runtime delete failed", "sandboxID", sandboxID, "err", err)
	}
}
