module github.com/vmihailenco/taskq/example/api_worker

go 1.15

require (
	github.com/go-redis/redis/v8 v8.11.4
	github.com/frain-dev/taskq/v3 v3.2.8
)

replace github.com/frain-dev/taskq/v3 => ../..
