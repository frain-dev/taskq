module github.com/vmihailenco/taskq/extra/taskqotel/v3

go 1.15

replace github.com/frain-dev/taskq/v3 => ../..

require (
	github.com/frain-dev/taskq/v3 v3.2.8
	go.opentelemetry.io/otel v1.0.0-RC2
	go.opentelemetry.io/otel/trace v1.0.0-RC2

)
