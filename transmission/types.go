package transmission

// TorrentStatus represents the status of a single torrent.
type TorrentStatus struct {
	ID            int
	Name          string
	Status        string // "downloading", "seeding", "stopped", "checking"
	Progress      float64 // 0-100
	Downloaded    int64   // bytes
	TotalSize     int64   // bytes
	DownloadSpeed int64   // bytes/s
	ETA           int64   // seconds, -1 if unknown
}

// Config holds Transmission RPC connection settings.
type Config struct {
	URL         string
	Username    string
	Password    string
	DefaultFolder string
}
