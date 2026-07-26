package cli

import (
	"io"

	"github.com/mdp/qrterminal/v3"
)

// printQR renders a link as a scannable block-character QR code.
//
// Recovery level L on purpose: a vless:// URI is long, and a higher level pushes the
// symbol to a version so large it stops fitting in an ordinary terminal window —
// at which point it is unscannable for a completely avoidable reason.
//
// Callers pass stderr. The code is decoration around the real output, and writing it to
// stdout would contaminate `SUB=$(vlessvmore user sub alice)` with a screenful of block
// characters.
func printQR(w io.Writer, link string) {
	qrterminal.GenerateWithConfig(link, qrterminal.Config{
		Writer:     w,
		Level:      qrterminal.L,
		HalfBlocks: true,
		// Explicit half-block glyphs rather than the defaults, so the code renders on
		// a light or dark terminal without inverting into unscannability.
		BlackChar:      qrterminal.BLACK_BLACK,
		WhiteChar:      qrterminal.WHITE_WHITE,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
		WhiteBlackChar: qrterminal.WHITE_BLACK,
		// Scanners need the quiet zone; without it many phones simply will not lock on.
		QuietZone: 1,
	})
}
