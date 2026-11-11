package mcp

import "context"

const (
	ResultBytes        = resultBytes
	MaximumSchemaBytes = maximumSchemaBytes
)

// WaitForRefreshes returns once no background refresh or session retirement is running, so a test
// can assert what a refresh it caused did.
func WaitForRefreshes(s *Service) {
	s.waitIdle()
}

var ErrRetired = errRetired

// OpenAfterReadingKey runs Execute's steps up to opening a session, with between run after the key
// was read and before the session opens, so a test can remove the key in that window.
func OpenAfterReadingKey(s *Service, principalID, server string, between func()) error {
	p := s.byName[server]
	key := sessionKey{server: server, principal: principalID}
	gen := s.generation(key)
	cred, _, err := s.credentialFor(context.Background(), principalID, p.server)
	if err != nil {
		return err
	}
	between()
	_, release, err := s.clientFor(key, p.server, cred, gen)
	if err == nil {
		release()
	}
	return err
}
