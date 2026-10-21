package webrtc

import (
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// The read loops count into an atomic, not into Connection.Recv, so /api/streams
// only sees the total if MarshalJSON copies it across.
func TestMarshalJSONReportsBytesRecv(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	conn := NewConn(pc)
	t.Cleanup(func() { _ = conn.Close() })

	conn.recv.Add(1234)
	b, err := conn.MarshalJSON()
	require.NoError(t, err)
	require.True(t, strings.Contains(string(b), `"bytes_recv":1234`), string(b))
}
