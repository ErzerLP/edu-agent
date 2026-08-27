module github.com/edu-agent/edu-agent/server/contracttests/fakellm

go 1.26.6

require github.com/edu-agent/edu-agent/server v0.0.0

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.9.2 // indirect
	golang.org/x/text v0.39.0 // indirect
)

replace github.com/edu-agent/edu-agent/server => ../../server
