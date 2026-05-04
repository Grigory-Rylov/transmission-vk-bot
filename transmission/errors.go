package transmission

import "fmt"

// ErrTransmissionUnavailable indicates the Transmission daemon is unreachable.
var ErrTransmissionUnavailable = fmt.Errorf("Transmission is not available. Please check connection.")

// ErrInvalidMagnetLink indicates the provided magnet link format is invalid.
var ErrInvalidMagnetLink = fmt.Errorf("Invalid magnet link format. Use: magnet:?xt=urn:btih:...")

// ErrTorrentExists indicates a torrent with the same name/ID already exists.
var ErrTorrentExists = fmt.Errorf("Torrent already exists in Transmission.")

// ErrRPCError indicates a generic Transmission RPC error.
type ErrRPCError struct {
	Code    int
	Message string
}

func (e ErrRPCError) Error() string {
	return fmt.Sprintf("Transmission RPC error %d: %s", e.Code, e.Message)
}
