package link

import (
	"vlessvmore/internal/config"
	"vlessvmore/internal/store"
)

// ForUser builds a user's vless:// URI from the server config.
//
// It lives here rather than being duplicated in the API and the CLI so a change to
// the link format can only be made in one place.
func ForUser(cfg *config.Config, id *store.IdentityStore, u *store.User) (string, error) {
	pub, err := id.PublicKey()
	if err != nil {
		return "", err
	}
	return Build(Params{
		UUID:        u.UUID,
		Host:        cfg.Host,
		Port:        cfg.Port,
		SNI:         cfg.SNI,
		PublicKey:   pub,
		ShortID:     id.Get().ShortID,
		Flow:        cfg.FlowValue(),
		Fingerprint: cfg.Fingerprint,
		// The label clients show: the configured server name when there is one, else
		// the user's own name.
		Name: cfg.ClientLabel(u.Name),
	}), nil
}
