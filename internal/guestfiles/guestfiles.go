// Package guestfiles holds support files mounted into every sandbox at
// /opt/hermitator/lib alongside the guest binary.
package guestfiles

import "embed"

// FS contains the files.
//
//go:embed *.mjs
var FS embed.FS
