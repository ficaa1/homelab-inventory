package plugin

import "time"

// PackageHost is a read-only software projection. It never exposes inventory,
// settings, runtime metadata, credentials, or storage handles to consumers.
type PackageHost struct {
	Host        string       `json:"host"`
	CollectedAt time.Time    `json:"collected_at"`
	Sets        []PackageSet `json:"sets"`
}
type PackageDistro struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
	Codename  string `json:"codename"`
}
type Package struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Arch          string `json:"arch"`
	Source        string `json:"source"`
	SourceVersion string `json:"source_version"`
}
type PackageSet struct {
	Scope      string        `json:"scope"`
	ImageRef   string        `json:"image_ref"`
	Containers []string      `json:"containers"`
	Manager    string        `json:"manager"`
	Distro     PackageDistro `json:"distro"`
	Status     string        `json:"status"`
	Packages   []Package     `json:"packages"`
}
type PackageSource interface {
	SetPackageSource(func() ([]PackageHost, error))
}

func (r *Registry) BindPackageSource(source func() ([]PackageHost, error)) {
	e := r.entries["cve"]
	if e == nil {
		return
	}
	e.op.Lock()
	defer e.op.Unlock()
	if p, ok := e.plugin.(PackageSource); ok {
		p.SetPackageSource(source)
	}
}
