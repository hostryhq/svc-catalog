package gen

import (
	"embed"
	"io/fs"
	"sync"

	"github.com/hostryhq/go-auth"
	gsync "github.com/hostryhq/go-common/sync"
	"gopkg.in/yaml.v3"
)

//go:embed catalogue.json
var catalogueJSON []byte

//go:embed entities
var entitiesFS embed.FS

//go:embed cloud-intents.yaml
var cloudIntentsYAML []byte

// CloudIntent is one intent of the gateway registry (api-specs sync/intents/cloud.yaml) as the emit check needs it.
type CloudIntent struct {
	Owner              string
	Cloud              bool // device_kinds is exactly [cloud]
	Streams            []string
	AuthorisedOnlineBy []string
	Entities           map[string][]string // entity -> mutation kinds the intent lists
}

// CloudIntents returns the cloud-origin intent registry by intent name.
func CloudIntents() (map[string]CloudIntent, error) {
	var doc struct {
		Intents []struct {
			Intent             string              `yaml:"intent"`
			Owner              string              `yaml:"owner"`
			DeviceKinds        []string            `yaml:"device_kinds"`
			Streams            []string            `yaml:"streams"`
			AuthorisedOnlineBy []string            `yaml:"authorised_online_by"`
			Entities           map[string][]string `yaml:"entities"`
		} `yaml:"intents"`
	}
	if err := yaml.Unmarshal(cloudIntentsYAML, &doc); err != nil {
		return nil, err
	}
	out := map[string]CloudIntent{}
	for _, i := range doc.Intents {
		out[i.Intent] = CloudIntent{Owner: i.Owner, Cloud: len(i.DeviceKinds) == 1 && i.DeviceKinds[0] == "cloud", Streams: i.Streams,
			AuthorisedOnlineBy: i.AuthorisedOnlineBy, Entities: i.Entities}
	}
	return out, nil
}

// Catalogue returns the permission catalogue generated from api-specs/common/permissions.yaml.
func Catalogue() (*auth.Catalogue, error) { return auth.LoadCatalogue(catalogueJSON) }

// Registry returns the synced-entity registry (api-specs/entities) of the pinned spec commit: owners, streams,
// field policies and declared replicas. Catalog is the owner of the entities with owner catalog.
func Registry() (*gsync.Registry, error) {
	sub, err := fs.Sub(entitiesFS, "entities")
	if err != nil {
		return nil, err
	}
	return gsync.LoadRegistry(sub, ".")
}

var loaded = sync.OnceValues(Registry)

// MustRegistry is Registry, loaded once; the registry is embedded, so a failure is a broken build.
func MustRegistry() *gsync.Registry {
	r, err := loaded()
	if err != nil {
		panic("gen: embedded entity registry: " + err.Error())
	}
	return r
}
