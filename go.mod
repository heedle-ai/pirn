module github.com/heedle-ai/pirn

go 1.24.0

// Until the heedle-ai modules are published, build against sibling checkouts.
replace (
	github.com/heedle-ai/bobbin => ../bobbin
	github.com/heedle-ai/draft => ../draft
	github.com/heedle-ai/reed => ../reed
	github.com/heedle-ai/shuttle => ../shuttle
	github.com/heedle-ai/weft => ../weft
)

require (
	github.com/anthropics/anthropic-sdk-go v1.80.0
	github.com/heedle-ai/bobbin v0.0.0
	github.com/heedle-ai/draft v0.0.0
	github.com/heedle-ai/reed v0.0.0
	github.com/heedle-ai/shuttle v0.0.0
	github.com/heedle-ai/weft v0.0.0
	github.com/modelcontextprotocol/go-sdk v1.4.0
	go.temporal.io/sdk v1.41.1
)

require (
	github.com/alecthomas/participle/v2 v2.1.1 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/chromedp/cdproto v0.0.0-20250724212937-08a3db8b4327 // indirect
	github.com/chromedp/chromedp v0.14.2 // indirect
	github.com/chromedp/sysutil v1.1.0 // indirect
	github.com/containerd/typeurl v1.0.2 // indirect
	github.com/containers/storage v1.46.1 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/facebookgo/clock v0.0.0-20150410010913-600d898af40a // indirect
	github.com/go-json-experiment/json v0.0.0-20250725192818-e39067aee2d2 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/mock v1.6.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.3.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.22.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/moby/buildkit v0.10.6 // indirect
	github.com/moby/sys/mountinfo v0.6.2 // indirect
	github.com/nexus-rpc/sdk-go v0.6.0 // indirect
	github.com/openshift/imagebuilder v1.2.6-0.20231110114814-35a50d57f722 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/robfig/cron v1.2.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.temporal.io/api v1.62.2 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/net v0.41.0 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sync v0.16.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.27.0 // indirect
	golang.org/x/time v0.5.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240827150818-7e3bb234dfed // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240827150818-7e3bb234dfed // indirect
	google.golang.org/grpc v1.67.1 // indirect
	google.golang.org/protobuf v1.36.6 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
