package creds

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Since #2051 (de157eb) SecretString first collapses any URL userinfo to "***" and
// then replaces registered secrets wherever else they appear. The userinfo rule is
// a regexp over RFC 3986 userinfo characters, so it doesn't cover every case (an
// unescaped "@" in a password, for one).
func TestString(t *testing.T) {
	AddSecret("admin")
	AddSecret("pa$$word")

	s := SecretString("rtsp://admin:pa$$word@192.168.1.123/stream1")
	require.Equal(t, "rtsp://***@192.168.1.123/stream1", s)

	// no userinfo here, so only the registered secrets are replaced
	s = SecretString("ffmpeg:http://192.168.1.123/cgi?user=admin&pwd=pa$$word")
	require.Equal(t, "ffmpeg:http://192.168.1.123/cgi?user=***&pwd=***", s)

	// both rules in one string: the userinfo collapses, the query secret is replaced
	s = SecretString("rtsp://admin:pa$$word@192.168.1.123/stream1?pwd=pa$$word")
	require.Equal(t, "rtsp://***@192.168.1.123/stream1?pwd=***", s)
}

// Moved from pkg/core when de157eb replaced core.StripUserinfo with the same regexp
// inside SecretString. Uses no registered secret, so only the userinfo rule applies.
func TestStringStripsUserinfo(t *testing.T) {
	s := `streams:
  test:
    - ffmpeg:rtsp://username:password@10.1.2.3:554/stream1
    - ffmpeg:rtsp://10.1.2.3:554/stream1@#video=copy
`
	require.Equal(t, `streams:
  test:
    - ffmpeg:rtsp://***@10.1.2.3:554/stream1
    - ffmpeg:rtsp://10.1.2.3:554/stream1@#video=copy
`, SecretString(s))
}
