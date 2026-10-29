package inventory

import "time"

// Software is additive in schema v5. Status/error distinguish incomplete
// collection from a successful empty result; nil Software means not collected.
// Stored runs never contain Software: API ingest moves it into softstore.
type Software struct {
	CollectedAt  time.Time        `json:"collected_at"`
	DurationMS   int64            `json:"duration_ms"`
	Status       string           `json:"status"`
	Error        string           `json:"error,omitempty"`
	Host         PackageSet       `json:"host"`
	Images       []ImagePackages  `json:"images"`
	ImagesStatus string           `json:"images_status"`
	ImagesError  string           `json:"images_error,omitempty"`
	Runtimes     []RuntimeVersion `json:"runtimes"`
}

type Distro struct {
	ID        string `json:"id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Codename  string `json:"codename,omitempty"`
}

// PackageSet stores installed binary revisions verbatim, including epochs.
// Count is the number retained, including when a collection limit was reached.
type PackageSet struct {
	Manager     string    `json:"manager"`
	Distro      Distro    `json:"distro"`
	Status      string    `json:"status"` // ok, partial, unsupported, failed
	Error       string    `json:"error,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
	DurationMS  int64     `json:"duration_ms"`
	Count       int       `json:"count"`
	Packages    []Package `json:"packages"`
}

// Empty Source means the binary name; empty SourceVersion means its version.
type Package struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Arch          string `json:"arch,omitempty"`
	Source        string `json:"source,omitempty"`
	SourceVersion string `json:"source_version,omitempty"`
}

// ImagePackages samples one running container's writable root per image ID.
// SampleContainer identifies it; other containers sharing the image can differ.
// No immutable-image cache is used: the writable package database is reread.
type ImagePackages struct {
	ImageID         string           `json:"image_id"`
	ImageRef        string           `json:"image_ref"`
	RepoDigests     []string         `json:"repo_digests"`
	Containers      []string         `json:"containers"`
	SampleContainer string           `json:"sample_container"`
	Architecture    string           `json:"architecture,omitempty"`
	OS              string           `json:"os,omitempty"`
	PackageSet      PackageSet       `json:"package_set"`
	Runtimes        []RuntimeVersion `json:"runtimes"`
}

type RuntimeVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Scope   string `json:"scope"`
	Source  string `json:"source"`
}
