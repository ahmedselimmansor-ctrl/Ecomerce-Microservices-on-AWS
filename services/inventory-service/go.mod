module github.com/souq/inventory-service

go 1.25.0

replace github.com/souq/go-modelcheck => ../../libs/go-modelcheck

require (
	github.com/go-chi/chi/v5 v5.3.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/oklog/ulid/v2 v2.1.2
	github.com/segmentio/kafka-go v0.4.51
)

require (
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/souq/go-modelcheck v0.0.0
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)
