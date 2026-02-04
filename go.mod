// Go module definition for the RMM agent.
// This is the core agent that runs on managed Linux hosts,
// reporting statistics and executing commands from the central server.
module github.com/doughall/linuxrmm/agent

go 1.24.0

require (
	github.com/coreos/go-systemd/v22 v22.7.0
	github.com/creack/pty v1.1.24
	github.com/gorilla/websocket v1.5.3
	github.com/hashicorp/go-retryablehttp v0.7.8
	github.com/knadh/koanf/parsers/yaml v0.1.0
	github.com/knadh/koanf/providers/file v1.1.2
	github.com/knadh/koanf/v2 v2.1.2
	github.com/nats-io/nats.go v1.39.1
	github.com/nats-io/nkeys v0.4.9
	github.com/robfig/cron/v3 v3.0.1
	github.com/shirou/gopsutil/v4 v4.25.12
	go.etcd.io/bbolt v1.4.3
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/ebitengine/purego v0.9.1 // indirect
	github.com/fsnotify/fsnotify v1.7.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/go-viper/mapstructure/v2 v2.2.1 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/knadh/koanf/maps v0.1.1 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/tklauser/go-sysconf v0.3.16 // indirect
	github.com/tklauser/numcpus v0.11.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)
