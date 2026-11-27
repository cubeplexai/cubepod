package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/cubeplexai/cubepod/pkg/client"
)

// errVolumeMountsRequireAPIKey is returned when named volume mounts are requested
// without an API key; the gateway resolves volume names per API key.
var errVolumeMountsRequireAPIKey = errors.New("named volume mounts require an API key; set --api-key or CUBEPOD_API_KEY")

// volumeMountsFlag is a repeatable flag.Value collecting NAME:PATH volume mounts.
type volumeMountsFlag []client.SandboxVolumeMount

func (f *volumeMountsFlag) String() string {
	if f == nil {
		return ""
	}
	parts := make([]string, len(*f))
	for i, m := range *f {
		parts[i] = m.Name + ":" + m.Path
	}
	return strings.Join(parts, ",")
}

func (f *volumeMountsFlag) Set(value string) error {
	m, err := parseVolumeMount(value)
	if err != nil {
		return err
	}
	*f = append(*f, m)
	return nil
}

// parseVolumeMount parses a NAME:PATH spec, splitting on the first colon.
func parseVolumeMount(spec string) (client.SandboxVolumeMount, error) {
	name, mountPath, ok := strings.Cut(spec, ":")
	if !ok {
		return client.SandboxVolumeMount{}, fmt.Errorf("invalid volume mount %q: expected NAME:PATH", spec)
	}
	m := client.SandboxVolumeMount{Name: strings.TrimSpace(name), Path: strings.TrimSpace(mountPath)}
	if err := validateVolumeMount(m); err != nil {
		return client.SandboxVolumeMount{}, fmt.Errorf("invalid volume mount %q: %w", spec, err)
	}
	return m, nil
}

// validateVolumeMount performs client-side checks that do not require the gateway.
// Name syntax, duplicate paths, volume existence and volumeKey conflicts are
// validated by the gateway.
func validateVolumeMount(m client.SandboxVolumeMount) error {
	if m.Name == "" {
		return errors.New("volume name must not be empty")
	}
	if m.Path == "" {
		return errors.New("mount path must not be empty")
	}
	if !path.IsAbs(m.Path) {
		return fmt.Errorf("mount path %q must be absolute", m.Path)
	}
	return nil
}

// normalizeVolumeMounts trims surrounding whitespace from names and paths in
// place, matching the gateway's mount path normalization.
func normalizeVolumeMounts(mounts []client.SandboxVolumeMount) {
	for i := range mounts {
		mounts[i].Name = strings.TrimSpace(mounts[i].Name)
		mounts[i].Path = strings.TrimSpace(mounts[i].Path)
	}
}

func validateVolumeMounts(mounts []client.SandboxVolumeMount) error {
	for i, m := range mounts {
		if err := validateVolumeMount(m); err != nil {
			return fmt.Errorf("volumeMounts[%d]: %w", i, err)
		}
	}
	return nil
}

// parseSandboxCreateArgs parses `cubepod sandbox create` flags into a request.
func parseSandboxCreateArgs(args []string, stderr io.Writer) (client.CreateSandboxRequest, error) {
	fs := flag.NewFlagSet("sandbox create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tpl := fs.String("template", "cubeplex-ubuntu24-desktop", "Template ID")
	volKey := fs.String("volume-key", "", "Deterministic workspace volume key")
	var mounts volumeMountsFlag
	fs.Var(&mounts, "volume-mount", "Mount a named volume as NAME:PATH (repeatable; requires an API key)")
	if err := fs.Parse(args); err != nil {
		return client.CreateSandboxRequest{}, err
	}
	return client.CreateSandboxRequest{
		TemplateID:   *tpl,
		VolumeKey:    *volKey,
		VolumeMounts: []client.SandboxVolumeMount(mounts),
	}, nil
}
