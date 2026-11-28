//go:build !linux

package reportjob

import "os"

func trustedJournalFile(_ *os.File, _ os.FileInfo, _ RecoveryApproval) bool { return false }
