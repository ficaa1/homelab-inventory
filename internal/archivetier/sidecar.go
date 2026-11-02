package archivetier

type Sidecar struct {
	FileID     int    `json:"fileId"`
	EpisodeIDs []int  `json:"episodeIds,omitempty"`
	Suffix     string `json:"suffix"`
	Staged     string `json:"staged"`
	SHA256     string `json:"sha256"`
}
