module github.com/banyan-vcs/banyan/enterprise

go 1.27.0

require (
	github.com/banyan-vcs/banyan v0.0.0
	github.com/stretchr/testify v1.12.1
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

replace github.com/banyan-vcs/banyan => ../
