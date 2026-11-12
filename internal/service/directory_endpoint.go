package service

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const sharedProviderDirectory = "/run/kord-provider-address"

var invalidProviderAddress = errors.New("invalid account directory address")

// Both ordinary and traced confirmations use the same address and service credential.
func accountDirectoryURL(directory string) (string, error) {
	mode, selected := os.LookupEnv("KORD_PROVIDER_ENDPOINT_MODE")
	legacy, hasLegacy := os.LookupEnv("ORPHEUS_ACCOUNT_DIRECTORY_URL")
	if !selected || mode == "legacy" {
		address, err := url.Parse(legacy)
		if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil || address.Fragment != "" || address.RawQuery != "" {
			return "", invalidProviderAddress
		}
		return address.String(), nil
	}
	_, otherLegacy := os.LookupEnv("KORD_PROVIDER_URL")
	if mode != "shared-file" || hasLegacy || otherLegacy {
		return "", invalidProviderAddress
	}
	base, err := sharedProviderURL(directory)
	if err != nil {
		return "", err
	}
	return base + "/internal/v1/call", nil
}

func sharedProviderURL(directory string) (string, error) {
	initial, err := os.Lstat(directory)
	if err != nil || !initial.IsDir() {
		return "", invalidProviderAddress
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", invalidProviderAddress
	}
	defer root.Close()
	parent, err := root.Open(".")
	if err != nil {
		return "", invalidProviderAddress
	}
	defer parent.Close()
	folder, err := parent.Stat()
	if err != nil || !os.SameFile(initial, folder) {
		return "", invalidProviderAddress
	}
	var fs syscall.Statfs_t
	if syscall.Fstatfs(int(parent.Fd()), &fs) != nil {
		return "", invalidProviderAddress
	}
	entry, err := root.Lstat("url")
	if err != nil || !entry.Mode().IsRegular() {
		return "", invalidProviderAddress
	}
	// os.Root follows symlinks that remain inside the root. Keep its directory
	// reference for the inode check and reject any final symlink in the kernel.
	file, err := os.OpenFile(filepath.Join(directory, "url"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", invalidProviderAddress
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(entry, info) || !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 || info.Size() > 2048 {
		return "", invalidProviderAddress
	}
	fileStat, folderStat := info.Sys().(*syscall.Stat_t), folder.Sys().(*syscall.Stat_t)
	const readOnlyMount = 1 // ST_RDONLY on Linux, MNT_RDONLY on Darwin.
	rootManaged := fileStat.Uid == 0 && folderStat.Uid == 0 && folder.Mode().Perm()&0022 == 0
	if fileStat.Nlink != 1 || (fs.Flags&readOnlyMount == 0 && !rootManaged) {
		return "", invalidProviderAddress
	}
	raw, err := io.ReadAll(io.LimitReader(file, 2049))
	if err != nil || len(raw) > 2048 {
		return "", invalidProviderAddress
	}
	value := strings.TrimSuffix(string(raw), "\n")
	for _, c := range value {
		if c < 33 || c > 126 {
			return "", invalidProviderAddress
		}
	}
	u, err := url.Parse(value)
	if err != nil || strings.ContainsAny(value, "?#\\") || u.Hostname() == "" || u.User != nil || (u.EscapedPath() != "" && u.EscapedPath() != "/") || strings.HasSuffix(u.Host, ":") {
		return "", invalidProviderAddress
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return "", invalidProviderAddress
	}
	if port := u.Port(); port != "" {
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return "", invalidProviderAddress
		}
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return "", invalidProviderAddress
		}
	}
	return strings.TrimSuffix(value, "/"), nil
}
