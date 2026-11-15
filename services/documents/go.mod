module github.com/Measured-Health/mhai-multiagent/services/documents

go 1.26.0

require (
	github.com/Measured-Health/mhai-multiagent/services/gokit v0.0.0
	github.com/jackc/pgx/v5 v5.12.0
	github.com/nats-io/nats.go v1.55.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/klauspost/compress v1.20.0 // indirect
	github.com/nats-io/nkeys v0.4.16 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.44.0 // indirect
)

// gokit is the module the documents service shares with core and the emr
// service (ADR 0014, ADR 0016). It is always built from this repository,
// never fetched. The documents module requires neither core's module nor
// the emr service's (TestDocumentsImportsNoOtherService).
replace github.com/Measured-Health/mhai-multiagent/services/gokit => ../gokit
