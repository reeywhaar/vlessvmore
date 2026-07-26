package link

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

// RecommendedQuietZone is the margin, in modules, that a renderer should leave around
// the matrix. Four is what the QR specification calls for; many scanners fail to lock
// on without it, and that failure looks like "the code is broken" rather than "the
// code has no margin".
const RecommendedQuietZone = 4

// QR is a QR code as a plain bit matrix, for clients that want to draw it themselves.
//
// Returning the modules rather than a PNG keeps the API free of image encoding and
// lets a caller render at any scale, in any colours, as SVG, canvas or table cells —
// all of which a fixed-size raster would prevent.
type QR struct {
	// Size is the width and the height in modules; the matrix is always square.
	Size int `json:"size"`

	// Rows has Size entries, each a Size-character string of '0' (light) and '1'
	// (dark), top row first. Strings rather than nested arrays because [][]bool
	// triples the JSON for no added information.
	Rows []string `json:"rows"`

	// QuietZone is the margin the caller should add around the matrix, in modules.
	// It is not included in Size or Rows.
	QuietZone int `json:"quiet_zone"`
}

// Encode renders text as a QR matrix.
//
// Recovery level L, matching what the CLI draws: a vless:// URI is long, and a higher
// level pushes the symbol to a larger version for a payload that is transmitted over a
// reliable channel anyway.
func Encode(text string) (*QR, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil, fmt.Errorf("encode qr code: %w", err)
	}

	rows := make([]string, code.Size)
	var b strings.Builder
	b.Grow(code.Size)
	for y := range code.Size {
		b.Reset()
		for x := range code.Size {
			if code.Black(x, y) {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
		}
		rows[y] = b.String()
	}
	return &QR{Size: code.Size, Rows: rows, QuietZone: RecommendedQuietZone}, nil
}
