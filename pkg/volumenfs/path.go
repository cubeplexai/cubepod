package volumenfs

import (
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"
)

// ExportRef is a parsed MOUNT path /<leaseID>/<volumeID>.
type ExportRef struct {
	LeaseID  string
	VolumeID string
}

// ParseExportPath parses a MOUNT dirpath of exactly two components.
func ParseExportPath(dirpath string) (ExportRef, error) {
	p := path.Clean("/" + strings.TrimSpace(dirpath))
	if p == "/" || p == "." || p == "" {
		return ExportRef{}, fmt.Errorf("%w: %q", ErrInvalidExportPath, dirpath)
	}
	if strings.Contains(p, "\\") || strings.Contains(dirpath, "..") {
		return ExportRef{}, fmt.Errorf("%w: %q", ErrInvalidExportPath, dirpath)
	}
	trimmed := strings.Trim(p, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ExportRef{}, fmt.Errorf("%w: expected /<leaseID>/<volumeID>, got %q", ErrInvalidExportPath, dirpath)
	}
	if !ValidLeaseID(parts[0]) {
		return ExportRef{}, fmt.Errorf("%w: %q", ErrInvalidLeaseID, parts[0])
	}
	if !ValidVolumeID(parts[1]) {
		return ExportRef{}, fmt.Errorf("%w: %q", ErrInvalidVolumeID, parts[1])
	}
	return ExportRef{LeaseID: parts[0], VolumeID: parts[1]}, nil
}

// NFSTarget builds the guest nfs_target string <clusterIP>:/<leaseID>/<volumeID>.
func NFSTarget(clusterIP, leaseID, volumeID string) string {
	ip := strings.TrimSpace(clusterIP)
	if strings.Contains(ip, ":") && !strings.HasPrefix(ip, "[") {
		ip = "[" + ip + "]"
	}
	return fmt.Sprintf("%s:/%s/%s", ip, leaseID, volumeID)
}

// VolumeDataDir returns the on-disk data directory for an official volume.
func VolumeDataDir(volumesDir, volumeID string) string {
	return path.Join(volumesDir, volumeID, "data")
}

// NFSAddr formats "<clusterIP>:<port>" for CUBEPOD_VOLUME_NFS_ADDR, bracketing IPv6.
func NFSAddr(clusterIP string, port int) string {
	ip := strings.TrimSpace(strings.Trim(clusterIP, "[]"))
	if port <= 0 {
		port = DefaultNFSPort
	}
	return net.JoinHostPort(ip, strconv.Itoa(port))
}
