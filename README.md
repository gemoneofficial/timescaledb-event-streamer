# timescaledb-event-streamer

`timescaledb-event-streamer` is a command line program to create a stream of
CDC (Chance Data Capture) TimescaleDB Hypertable events from a PostgreSQL
installation running the TimescaleDB extension.

Change Data Capture is a technology where insert, update, delete and similar
operations inside the database generate a corresponding set of events, which
are commonly distributed through a messaging connector, such as Kafka, NATS,
or similar.

# Getting Started

`timescaledb-event-streamer` requires the [Go runtime (version 1.20+)](https://go.dev/doc/install)
to be installed. With this requirement satisfied, the installation can be
kicked off using:

```bash
$ go install github.com/noctarius/timescaledb-event-streamer/cmd/timescaledb-event-streamer@latest
```

Before using the program, a configuration file needs to be created. An example
configuration can be
found [here](https://raw.githubusercontent.com/noctarius/timescaledb-event-streamer/main/config.example.toml).

For a full reference of the existing configuration options, see the [Configuration](#configuration)
section.

## Using timescaledb-event-streamer

After creating a configuration file, `timescaledb-event-streamer` can be executed
with the following command:

```bash
$ timescaledb-event-streamer -config=./config.toml
```

The tool will connect to your TimescaleDB database, and start replicating incoming
events.

# Configuration

`timescaledb-event-streamer` utilizes [TOML](https://toml.io/en/v1.0.0) as its
configuration file format due to its simplicity.

The actual configuration values are designed as canonical name (dotted keys).

## PostgreSQL Configuration

| Property                |                                               Description | Data Type |                                 Default Value |
|-------------------------|----------------------------------------------------------:|----------:|----------------------------------------------:|
| `postgresql.connection` | The connection string in one of the libpq-supported forms |    string | host=localhost user=repl_user sslmode=disable |
| `postgresql.password`   |                      The password to connect to the user. |    string |            Environment variable: `PGPASSWORD` |

## Topic Configuration

| Property                    |                                                                               Description | Data Type | Default Value |
|-----------------------------|------------------------------------------------------------------------------------------:|----------:|--------------:|
| `topic.namingstrategy.type` | The naming strategy of topic names. At the moment only the value `debezium` is supported. |    string |    `debezium` |
| `topic.prefix`              |                                                            The prefix for all topic named |    string | `timescaledb` |

## TimescaleDB Configuration

| Property                           |                                                                                                                                                                                                                                      Description |        Data Type | Default Value |
|------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------:|-----------------:|--------------:|
| `timescaledb.hypertables.includes` | The includes definition defines which hypertables to include in the event stream generation. The available patters are explained in [Includes and Excludes Patterns](#includes-and-excludes-patterns). . Excludes have precedence over includes. | array of strings |   empty array |
| `timescaledb.hypertables.excludes` |   The excludes definition defines which hypertables to exclude in the event stream generation. The available patters are explained in [Includes and Excludes Patterns](#includes-and-excludes-patterns). Excludes have precedence over includes. | array of strings |   empty array |
| `timescaledb.events.read`          |                                                                                                                                                                                                The property defines if read events are generated |          boolean |          true |
| `timescaledb.events.insert`        |                                                                                                                                                                                              The property defines if insert events are generated |          boolean |          true |
| `timescaledb.events.update`        |                                                                                                                                                                                              The property defines if update events are generated |          boolean |          true |
| `timescaledb.events.delete`        |                                                                                                                                                                                              The property defines if delete events are generated |          boolean |          true |
| `timescaledb.events.truncate`      |                                                                                                                                                                                            The property defines if truncate events are generated |          boolean |          true |
| `timescaledb.events.compression`   |                                                                                                                                                                                         The property defines if compression events are generated |          boolean |         false |
| `timescaledb.events.decompression` |                                                                                                                                                                                       The property defines if decompression events are generated |          boolean |         false |

## Sink Configuration

| Property                            |                                                                                       Description |        Data Type | Default Value |
|-------------------------------------|--------------------------------------------------------------------------------------------------:|-----------------:|--------------:|
| `sink.type`                         | The property defines which sink adapter is to be used. Valid values are `stdout`, `nats`, `kafka` |           string |      `stdout` |
| `sink.nats.address`                 |                   The NATS connection address, according to the NATS connection string definition |           string |  empty string |
| `sink.nats.authorization`           |                   The NATS authorization type. Valued values are `userinfo`, `credentials`, `jwt` |           string |  empty string |
| `sink.nats.userinfo.username`       |                                                    The username of userinfo authorization details |           string |  empty string | 
| `sink.nats.userinfo.password`       |                                                    The password of userinfo authorization details |           string |  empty string | 
| `sink.nats.credentials.certificate` |                             The path of the certificate file of credentials authorization details |           string |  empty string | 
| `sink.nats.credentials.seeds`       |                                   The paths of seeding files of credentials authorization details | array of strings |   empty array | 

# Includes and Excludes Patterns

Includes and Excludes can be defined as fully canonical references to hypertables
(equivalent to [PostgreSQL regclass definition](https://www.postgresql.org/docs/15/datatype-oid.html))
or as patterns with wildcards.

For an example we assume the following hypertables to exist.

| Schema    |      Hypertable |         Canonical Name |
|-----------|----------------:|-----------------------:|
| public    |         metrics |         public.metrics |
| public    | status_messages | public.status_messages |
| invoicing |        invoices |     invoicing.invoices |
| alarming  |          alarms |        alarming.alarms |

To note, excludes have precedence over includes, meaning, that if both includes and
excludes match a specific hypertable, the hypertable will be excluded from the event
generation process.

Hypertables can be referred to by their canonical name (dotted notation of schema and
hypertable table name).
`timescaledb.hypertables.includes = [ 'public.metrics', 'invoicing.invoices' ]`

When referring to the hypertable by its canonical name, the matcher will only match
the exact hypertable. That said, the above example will yield events for the hypertables
`public.metrics` and  `invoicing.invoices` but none of the other ones.

## Wildcards

Furthermore, includes and excludes can utilize wildcard characters to match a subset
of tables based on the provided pattern.

`timescaledb-event-streamer` understands 3 types of wildcards:

| Wildcard |                                                   Description |
|----------|--------------------------------------------------------------:|
| *        |    The asterisk (*) character matches zero or more characters |
| +        |         The plus (+) character matches one or more characters |
| ?        | The question mark (?) character matches exactly one character |

Wildcards can be used in the schema or table names. It is also possible to have them
in schema and table names at the same time.

### Asterisk: Zero Or More Matching

| Schema |      Hypertable |         Canonical Name |
|--------|----------------:|-----------------------:|
| public |         metrics |         public.metrics |
| public | status_messages | public.status_messages |

`timescaledb.hypertables.includes = [ 'public.*' ]` matches all hypertables in schema
`public`.

| Schema |      Hypertable |         Canonical Name |
|--------|----------------:|-----------------------:|
| public |       status_1h |       public.status_1h |
| public |      status_12h |      public.status_12h |
| public | status_messages | public.status_messages |

`timescaledb.hypertables.includes = [ 'public.statis_1*' ]` matches `public.status_1h`
and `public.status_12h`, but not `public.status_messages`.

| Schema    | Hypertable |    Canonical Name |
|-----------|-----------:|------------------:|
| customer1 |    metrics | customer1.metrics |
| customer2 |    metrics | customer2.metrics |

Accordingly, it is possible to match a specific hypertable in all customer schemata
using a pattern such as `timescaledb.hypertables.includes = [ 'customer*.metrics' ]`.

# Plus: One or More Matching

| Schema |     Hypertable |         Canonical Name |
|--------|---------------:|-----------------------:|
| public |   status_1_day |    public.status_1_day |
| public | status_1_month |  public.status_1_month |
| public |  status_1_year | public.status_12_month |

`timescaledb.hypertables.includes = [ 'public.statis_+_month' ]` matches
hypertables `public.status_1_month` and `public.status_12_month`, but not
`public.status_1_day`.

| Schema    | Hypertable |    Canonical Name |
|-----------|-----------:|------------------:|
| customer1 |    metrics | customer1.metrics |
| customer2 |    metrics | customer2.metrics |

Accordingly, it is possible to match a specific hypertable in all customer schemata
using a pattern such as `timescaledb.hypertables.includes = [ 'customer+.metrics' ]`.

# Question Mark: Exactly One Matching

| Schema |    Hypertable |       Canonical Name |
|--------|--------------:|---------------------:|
| public |  status_1_day |  public.status_1_day |
| public |  status_7_day |  public.status_7_day |
| public | status_14_day | public.status_14_day |

`timescaledb.hypertables.includes = [ 'public.statis_?_day' ]` matches
hypertables `public.status_1_day` and `public.status_7_day`, but not
`public.status_14_day`.
