// Copyright the LoadWave Authors.
// SPDX-License-Identifier: AGPL-3.0-or-later

package loadwave

import "syscall"

// Socket errors worth distinguishing in the failure breakdown.
//
// They are aliased here rather than referenced inline so that classifyError
// reads as a flat list of cases, and so a platform that spells one of them
// differently only needs a build-tagged copy of this file.
var (
	syscallConnRefused error = syscall.ECONNREFUSED
	syscallConnReset   error = syscall.ECONNRESET
)
