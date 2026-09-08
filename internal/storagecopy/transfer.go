package storagecopy

import (
	"fmt"
	"strconv"
	"strings"

	transferrsync "github.com/migtools/pvc-transfer/transfer/rsync"

	migrationv1alpha1 "github.com/openshift/vcf-migration-operator/api/v1alpha1"
)

// BuildRsyncOptions constructs the authoritative option set for an attempt.
// The caller controls whether this is a non-destructive precopy or final sync.
func BuildRsyncOptions(fidelity migrationv1alpha1.FileCopyFidelity, final bool, bandwidthLimit string) (*transferrsync.CommandOptions, error) {
	opts := &transferrsync.CommandOptions{
		Recursive: true, SymLinks: true, Permissions: true, ModTimes: true,
		HardLinks: true, Partial: true, HumanReadable: true,
		Extras: []string{"--sparse"},
		Info:   []string{"COPY2", "DEL2", "REMOVE2", "SKIP2", "FLIST2", "PROGRESS2", "STATS2"},
	}
	switch fidelity {
	case migrationv1alpha1.FileCopyFidelityRestricted:
	case migrationv1alpha1.FileCopyFidelityExact:
		opts.Owners = true
		opts.Groups = true
		opts.Extras = append(opts.Extras, "--acls", "--xattrs")
	default:
		return nil, fmt.Errorf("unsupported filesystem fidelity %q", fidelity)
	}
	if final {
		opts.Delete = true
		opts.Extras = append(opts.Extras, "--checksum", "--delete-delay")
	}
	if bandwidthLimit != "" {
		value := strings.TrimSuffix(strings.TrimSpace(bandwidthLimit), "Ki")
		limit, err := strconv.Atoi(value)
		if err != nil || limit <= 0 {
			return nil, fmt.Errorf("bandwidthLimit must be a positive KiB/s integer, got %q", bandwidthLimit)
		}
		opts.BwLimit = &limit
	}
	if _, err := opts.Options(); err != nil {
		return nil, fmt.Errorf("validating rsync options: %w", err)
	}
	return opts, nil
}
