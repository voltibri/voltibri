module github.com/voltibri/voltibri

go 1.26.5

require (
	github.com/simonvetter/modbus v1.6.4
	github.com/voltibri/voltibri/sdk v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/goburrow/serial v0.1.0 // indirect

replace github.com/voltibri/voltibri/sdk => ./sdk
