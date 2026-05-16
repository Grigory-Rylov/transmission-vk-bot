package transmission

import "path/filepath"

// TorrentStatus represents the status of a single torrent.
type TorrentStatus struct {
	ID            int
	Name          string
	Status        string // "downloading", "seeding", "stopped", "checking"
	Progress      float64 // 0-100
	Downloaded    int64   // bytes
	TotalSize     int64   // bytes
	DownloadSpeed int64   // bytes/s
	UploadSpeed   int64   // bytes/s
	UploadRatio   float64
	ETA           int64   // seconds, -1 if unknown
	DownloadDir   string
	IsFinished    bool
	LeftUntilDone int64
}

// SessionStats holds overall Transmission session statistics.
type SessionStats struct {
	ActiveTorrentCount int64
	PausedTorrentCount int64
	TorrentCount       int64
	DownloadSpeed      int64
	UploadSpeed        int64
	CurrentStats       Stats
	CumulativeStats    Stats
}

// Stats represents cumulative or current download/upload statistics.
type Stats struct {
	DownloadedBytes int64
	UploadedBytes   int64
	FilesAdded      int64
	SessionCount    int64
	SecondsActive   int64
}

// Config holds Transmission RPC connection settings.
type Config struct {
	URL         string
	Username    string
	Password    string
	DefaultFolder string
	// Categories maps category names to subdirectory paths
	Categories map[string]string
}

// GetDownloadDir returns the full download path for a given category.
func (c *Config) GetDownloadDir(category string) string {
	if c.Categories == nil {
		return c.DefaultFolder
	}
	if dir, ok := c.Categories[category]; ok {
		return filepath.Join(c.DefaultFolder, dir)
	}
	if other, ok := c.Categories["other"]; ok {
		return filepath.Join(c.DefaultFolder, other)
	}
	return c.DefaultFolder
}
