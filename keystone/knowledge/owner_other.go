//go:build !unix

package knowledge

import "io/fs"

const noFollow = 0

func ownerOfFile(fs.FileInfo) (int, int, bool) { return 0, 0, false }
