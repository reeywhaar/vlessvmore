package api

// Platform is one entry in the install page's OS switch.
//
// URLs live here rather than in the locale files so a translation cannot break a link, and
// so adding a language never means re-checking store URLs. Screenshots are per-platform for
// the same reason the switch exists at all: the day the Android app diverges from the iOS
// one, only this list changes.
type Platform struct {
	ID          string
	StoreURL    string
	DirectURL   string // A download outside the store, when there is one.
	Screenshots []string
}

// platforms is the OS switch, in display order. The first entry is the fallback when the
// user agent says nothing useful.
//
// Both entries share the iOS captures: Hiddify is one Flutter app and the screens are the
// same apart from the system status bar. Real Android captures replace the list here.
var platforms = []Platform{
	{
		ID:          "ios",
		StoreURL:    "https://apps.apple.com/app/hiddify-proxy-vpn/id6596777532",
		Screenshots: hiddifyShots,
	},
	{
		ID:          "android",
		StoreURL:    "https://play.google.com/store/apps/details?id=app.hiddify.com",
		DirectURL:   "https://github.com/hiddify/hiddify-app/releases",
		Screenshots: hiddifyShots,
	},
}

// hiddifyShots names the images each step refers to, in the order the page uses them.
var hiddifyShots = []string{"add-plus", "add-clipboard", "add-qr", "connect", "connected"}

// shotWidth and shotHeight are what the page tells the browser to reserve, so the layout
// does not jump once the images arrive. Every capture is the same phone with the status
// bar cropped off, so one pair covers all of them, and TestScreenshotsMatchDeclaredSize
// fails if a regenerated image stops agreeing.
const (
	shotWidth  = 540
	shotHeight = 1109
)

func platformByID(id string) (Platform, bool) {
	for _, p := range platforms {
		if p.ID == id {
			return p, true
		}
	}
	return Platform{}, false
}
