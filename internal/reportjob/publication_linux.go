//go:build linux

package reportjob

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func trustedJournalFile(f *os.File, st os.FileInfo, a RecoveryApproval) bool {
	stat := st.Sys().(*syscall.Stat_t)
	flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	const appendOnly = 0x20 // FS_APPEND_FL, linux/fs.h
	return err == nil && flags&appendOnly != 0 && stat.Uid == 0 && int(stat.Gid) == os.Getegid() &&
		stat.Nlink == 1 && stat.Dev == a.Device && stat.Ino == a.Inode
}
