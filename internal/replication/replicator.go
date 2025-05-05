/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements. See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package replication

import (
	"bytes"
	stderrors "errors"
	"fmt"
	"github.com/go-errors/errors"
	"github.com/jackc/pgx/v5"
	"github.com/noctarius/timescaledb-event-streamer/internal/erroring"
	"github.com/noctarius/timescaledb-event-streamer/internal/eventing/eventemitting"
	"github.com/noctarius/timescaledb-event-streamer/internal/logging"
	"github.com/noctarius/timescaledb-event-streamer/internal/replication/replicationchannel"
	"github.com/noctarius/timescaledb-event-streamer/internal/stats"
	"github.com/noctarius/timescaledb-event-streamer/internal/sysconfig"
	"github.com/noctarius/timescaledb-event-streamer/internal/systemcatalog/snapshotting"
	"github.com/noctarius/timescaledb-event-streamer/spi/config"
	"github.com/noctarius/timescaledb-event-streamer/spi/encoding"
	"github.com/noctarius/timescaledb-event-streamer/spi/pgtypes"
	"github.com/noctarius/timescaledb-event-streamer/spi/publication"
	"github.com/noctarius/timescaledb-event-streamer/spi/replicationcontext"
	"github.com/noctarius/timescaledb-event-streamer/spi/sidechannel"
	"github.com/noctarius/timescaledb-event-streamer/spi/statestorage"
	"github.com/noctarius/timescaledb-event-streamer/spi/systemcatalog"
	"github.com/noctarius/timescaledb-event-streamer/spi/task"
	"github.com/noctarius/timescaledb-event-streamer/spi/wiring"
	"github.com/samber/lo"
	"github.com/urfave/cli"
	"slices"
)

const esPreviouslyKnownChunks = "::previously::known::chunks"
const esPreviouslyKnownTables = "::previously::known::tables"

// Replicator is the main controller for all things logical replication,
// such as the logical replication connection, the side channel connection,
// and other services necessary to run the event stream generation.
type Replicator struct {
	logger        *logging.Logger
	config        *sysconfig.SystemConfig
	shutdownTasks []func() error
}

// NewReplicator instantiates a new instance of the Replicator.
func NewReplicator(
	config *sysconfig.SystemConfig,
) (*Replicator, error) {

	logger, err := logging.NewLogger("Replicator")
	if err != nil {
		return nil, err
	}

	return &Replicator{
		logger: logger,
		config: config,
	}, nil
}

// StartReplication initiates the actual replication process
func (r *Replicator) StartReplication() *cli.ExitError {
	r.shutdownTasks = nil
	container, err := wiring.NewContainer(
		StaticModule,
		DynamicModule,
		wiring.DefineModule("Config", func(module wiring.Module) {
			module.Provide(func() *config.Config {
				return r.config.Config
			})
			module.Provide(func() *pgx.ConnConfig {
				return r.config.PgxConfig
			})
			module.Invoke(r.containerInitializer)
		}),
		OverridesModule(r.config),
	)
	if err != nil {
		return erroring.AdaptError(err, 1)
	}

	// Start statistics service
	var statsService *stats.Service
	if err := container.Service(&statsService); err != nil {
		return erroring.AdaptError(err, 1)
	}
	if err := statsService.Start(); err != nil {
		return erroring.AdaptError(err, 0)
	}
	r.shutdownTasks = append(r.shutdownTasks, func() error {
		return statsService.Stop()
	})

	// Start internal dispatching
	var taskManager task.TaskManager
	if err := container.Service(&taskManager); err != nil {
		return erroring.AdaptError(err, 1)
	}
	taskManager.StartDispatcher()
	r.shutdownTasks = append(r.shutdownTasks, func() error {
		return taskManager.StopDispatcher()
	})

	// Start Replication context
	var replicationContext replicationcontext.ReplicationContext
	if err := container.Service(&replicationContext); err != nil {
		return erroring.AdaptError(err, 1)
	}
	if err := replicationContext.StartReplicationContext(); err != nil {
		return erroring.AdaptErrorWithMessage(err, "failed to start replication context", 18)
	}
	r.shutdownTasks = append(r.shutdownTasks, func() error {
		return replicationContext.StopReplicationContext()
	})

	// Start event emitter
	var eventEmitter *eventemitting.EventEmitter
	if err := container.Service(&eventEmitter); err != nil {
		return erroring.AdaptError(err, 1)
	}

	if err := eventEmitter.Start(); err != nil {
		return erroring.AdaptErrorWithMessage(err, "failed to start event emitter", 24)
	}
	r.shutdownTasks = append(r.shutdownTasks, func() error {
		return eventEmitter.Stop()
	})

	// Start the snapshotter
	var snapshotter *snapshotting.Snapshotter
	if err := container.Service(&snapshotter); err != nil {
		return erroring.AdaptError(err, 1)
	}
	snapshotter.StartSnapshotter()
	r.shutdownTasks = append(r.shutdownTasks, func() error {
		snapshotter.StopSnapshotter()
		return nil
	})

	var publicationManager publication.PublicationManager
	if err := container.Service(&publicationManager); err != nil {
		return erroring.AdaptError(err, 1)
	}

	var systemCatalog systemcatalog.SystemCatalog
	if err := container.Service(&systemCatalog); err != nil {
		return erroring.AdaptError(err, 1)
	}
	if issues := r.checkReplicaIdentities(systemCatalog); issues != nil {
		r.logger.Errorln("Replica Identity issues found:")
		for _, issue := range issues {
			r.logger.Errorf("\t* %s", issue)
		}
		return cli.NewExitError("stopped", 1)
	}

	var stateStorageManager statestorage.Manager
	if err := container.Service(&stateStorageManager); err != nil {
		return erroring.AdaptError(err, 1)
	}
	r.shutdownTasks = append(r.shutdownTasks, func() error {
		state, err1 := encodeKnownTables(systemCatalog.GetAllChunks())
		if err1 == nil {
			stateStorageManager.SetEncodedState(esPreviouslyKnownChunks, state)
		}
		state, err2 := encodeKnownTables(systemCatalog.GetAllVanillaTables())
		if err2 == nil {
			stateStorageManager.SetEncodedState(esPreviouslyKnownTables, state)
		}
		if err1 != nil || err2 != nil {
			return stderrors.Join(err1, err2)
		}
		return nil
	})

	publishedTables, err := publicationManager.ReadPublishedTables()
	if err != nil {
		return erroring.AdaptErrorWithMessage(err, "failed to read published tables", 25)
	}

	// Get initial list of chunks to add to publication
	initialTables, err := r.collectChunksForPublication(
		stateStorageManager.EncodedState, systemCatalog.GetAllChunks, publishedTables,
	)
	if err != nil {
		return erroring.AdaptErrorWithMessage(err, "failed to read known chunks", 25)
	}

	// Get list of vanilla tables to add to publication
	initialVanillaTables, err := r.collectVanillaTablesForPublication(
		stateStorageManager.EncodedState, systemCatalog.GetAllVanillaTables, publishedTables,
	)
	if err != nil {
		return erroring.AdaptErrorWithMessage(err, "failed to read known chunks", 25)
	}
	initialTables = append(initialTables, initialVanillaTables...)

	var replicationChannel *replicationchannel.ReplicationChannel
	if err := container.Service(&replicationChannel); err != nil {
		return erroring.AdaptError(err, 1)
	}
	if err := replicationChannel.StartReplicationChannel(initialTables); err != nil {
		if errors.Is(err, sidechannel.ErrNoRestartPointInReplicationSlot) {
			return erroring.AdaptErrorWithMessage(err,
				"No restart LSN available in replication slot. Cannot resume, replicated data would have gaps.",
				30,
			)
		}
		return erroring.AdaptError(err, 16)
	}
	r.shutdownTasks = append(r.shutdownTasks, func() error {
		return replicationChannel.StopReplicationChannel()
	})

	return nil
}

// StopReplication initiates a clean shutdown of the replication process. This
// call blocks until the shutdown process has finished.
func (r *Replicator) StopReplication() *cli.ExitError {
	if len(r.shutdownTasks) > 0 {
		errors := make([]error, 0)
		for i := len(r.shutdownTasks) - 1; i >= 0; i-- {
			if err := r.shutdownTasks[i](); err != nil {
				errors = append(errors, err)
			}
		}
		if len(errors) > 0 {
			return erroring.AdaptError(stderrors.Join(errors...), 250)
		}
	}
	return nil
}

func (r *Replicator) checkReplicaIdentities(
	systemCatalog systemcatalog.SystemCatalog,
) []string {

	issues := make([]string, 0)
	for _, hypertable := range systemCatalog.GetAllHypertables() {
		table := hypertable.(*systemcatalog.Hypertable)

		if table.IsContinuousAggregate() {
			continue
		}

		if table.ReplicaIdentity() == pgtypes.FULL {
			continue
		}

		columns := table.Columns()
		if table.ReplicaIdentity() == pgtypes.INDEX && !columns.HasReplicaIdentity() {
			issues = append(issues, fmt.Sprintf(
				"Hypertable %s has replica identity INDEX, but no valid index", table.CanonicalName(),
			))
			continue
		}

		if columns.HasPrimaryKey() {
			continue
		}

		issues = append(issues, fmt.Sprintf(
			"Hypertable %s has replica identity DEFAULT, but no valid primary key", table.CanonicalName(),
		))
	}

	for _, vanillaTable := range systemCatalog.GetAllVanillaTables() {
		table := vanillaTable.(systemcatalog.BaseTable)

		if table.ReplicaIdentity() == pgtypes.FULL {
			continue
		}

		columns := table.Columns()
		if table.ReplicaIdentity() == pgtypes.INDEX && !columns.HasReplicaIdentity() {
			issues = append(issues, fmt.Sprintf(
				"Table %s has replica identity INDEX, but no valid index", table.CanonicalName(),
			))
			continue
		}

		if columns.HasPrimaryKey() {
			continue
		}

		issues = append(issues, fmt.Sprintf(
			"Table %s has replica identity DEFAULT, but no valid primary key", table.CanonicalName(),
		))
	}

	if len(issues) > 0 {
		return issues
	}
	return nil
}

func (r *Replicator) containerInitializer(
	replicationContext replicationcontext.ReplicationContext, typeManager pgtypes.TypeManager,
) error {

	logger, err := logging.NewLogger("Replicator")
	if err != nil {
		return erroring.AdaptError(err, 1)
	}

	// Check version information
	if !replicationContext.IsMinimumPostgresVersion() {
		return cli.NewExitError(
			"timescaledb-event-streamer requires PostgreSQL 13 or later", 11,
		)
	}
	if !replicationContext.IsMinimumTimescaleVersion() {
		return cli.NewExitError(
			"timescaledb-event-streamer requires TimescaleDB 2.10 or later", 12,
		)
	}

	// Check WAL replication level
	if !replicationContext.IsLogicalReplicationEnabled() {
		return cli.NewExitError(
			"timescaledb-event-streamer requires wal_level set to 'logical'", 16,
		)
	}

	// Log system information
	logger.Infof("Discovered System Information:")
	logger.Infof("  * PostgreSQL version %s", replicationContext.PostgresVersion())
	logger.Infof("  * TimescaleDB version %s", replicationContext.TimescaleVersion())
	logger.Infof("  * PostgreSQL System Identity %s", replicationContext.SystemId())
	logger.Infof("  * PostgreSQL Timeline %d", replicationContext.Timeline())
	logger.Infof("  * PostgreSQL DatabaseName %s", replicationContext.DatabaseName())
	logger.Infof("  * PostgreSQL Types loaded %d", typeManager.NumKnownTypes())
	return nil
}

func (r *Replicator) collectVanillaTablesForPublication(
	encodedState func(name string) ([]byte, bool),
	getAllVanillaTables func() []systemcatalog.SystemEntity,
	publishedTables []systemcatalog.SystemEntity,
) ([]systemcatalog.SystemEntity, error) {

	allKnownTables, err := getKnownVanillaTables(encodedState, getAllVanillaTables)
	if err != nil {
		return nil, erroring.AdaptErrorWithMessage(err, "failed to read known tables", 25)
	}

	r.logger.Debugf(
		"All interesting tables: %+v",
		lo.Map(allKnownTables, extractCanonicalNameMapper),
	)

	// Filter out published chunks, we're only interested in non TimescaleDB tables
	publishedTables = lo.Filter(publishedTables, func(item systemcatalog.SystemEntity, _ int) bool {
		return item.SchemaName() != "_timescaledb_internal" && item.SchemaName() != "_timescaledb_catalog"
	})

	r.logger.Debugf(
		"Tables already in publication: %+v",
		lo.Map(publishedTables, extractCanonicalNameMapper),
	)

	initialTables := lo.Filter(allKnownTables, func(item systemcatalog.SystemEntity, _ int) bool {
		return !slices.ContainsFunc(publishedTables, func(other systemcatalog.SystemEntity) bool {
			return item.CanonicalName() == other.CanonicalName()
		})
	})
	r.logger.Debugf(
		"Tables to be added publication: %+v",
		lo.Map(initialTables, extractCanonicalNameMapper),
	)
	return initialTables, nil
}

func (r *Replicator) collectChunksForPublication(
	encodedState func(name string) ([]byte, bool),
	getAllChunks func() []systemcatalog.SystemEntity,
	publishedTables []systemcatalog.SystemEntity,
) ([]systemcatalog.SystemEntity, error) {

	// Get initial list of chunks to add to publication
	allKnownTables, err := getKnownChunks(encodedState, getAllChunks)
	if err != nil {
		return nil, erroring.AdaptErrorWithMessage(err, "failed to read known chunks", 25)
	}

	r.logger.Debugf(
		"All interesting chunks: %+v",
		lo.Map(allKnownTables, extractCanonicalNameMapper),
	)

	// Filter published chunks to only add new chunks
	publishedTables = lo.Filter(publishedTables, func(item systemcatalog.SystemEntity, _ int) bool {
		return item.SchemaName() == "_timescaledb_internal"
	})

	r.logger.Debugf(
		"Chunks already in publication: %+v",
		lo.Map(publishedTables, extractCanonicalNameMapper),
	)

	initialChunkTables := lo.Filter(allKnownTables, func(item systemcatalog.SystemEntity, _ int) bool {
		return !slices.ContainsFunc(publishedTables, func(other systemcatalog.SystemEntity) bool {
			return item.CanonicalName() == other.CanonicalName()
		})
	})
	r.logger.Debugf(
		"Chunks to be added publication: %+v",
		lo.Map(initialChunkTables, extractCanonicalNameMapper),
	)
	return initialChunkTables, nil
}

func getKnownVanillaTables(
	encodedState func(name string) ([]byte, bool),
	getAllVanillaTables func() []systemcatalog.SystemEntity,
) ([]systemcatalog.SystemEntity, error) {

	allTables := getAllVanillaTables()
	if state, present := encodedState(esPreviouslyKnownTables); present {
		candidates, err := decodeKnownTables(state)
		if err != nil {
			return nil, err
		}

		// Filter potentially deleted chunks
		return lo.Filter(candidates, func(item systemcatalog.SystemEntity, index int) bool {
			return slices.ContainsFunc(allTables, func(other systemcatalog.SystemEntity) bool {
				return item.CanonicalName() == other.CanonicalName()
			})
		}), nil
	}

	return allTables, nil
}

func getKnownChunks(
	encodedState func(name string) ([]byte, bool),
	getAllChunks func() []systemcatalog.SystemEntity,
) ([]systemcatalog.SystemEntity, error) {

	allChunks := getAllChunks()
	if state, present := encodedState(esPreviouslyKnownChunks); present {
		candidates, err := decodeKnownTables(state)
		if err != nil {
			return nil, err
		}

		// Filter potentially deleted chunks
		return lo.Filter(candidates, func(item systemcatalog.SystemEntity, index int) bool {
			return slices.ContainsFunc(allChunks, func(other systemcatalog.SystemEntity) bool {
				return item.CanonicalName() == other.CanonicalName()
			})
		}), nil
	}

	return allChunks, nil
}

func decodeKnownTables(
	data []byte,
) ([]systemcatalog.SystemEntity, error) {

	buffer := encoding.NewReadBuffer(bytes.NewBuffer(data))

	numOfChunks, err := buffer.ReadUint32()
	if err != nil {
		return nil, errors.Wrap(err, 0)
	}

	chunks := make([]systemcatalog.SystemEntity, 0, numOfChunks)
	for i := 0; i < int(numOfChunks); i++ {
		schemaName, err := buffer.ReadString()
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}
		tableName, err := buffer.ReadString()
		if err != nil {
			return nil, errors.Wrap(err, 0)
		}
		chunks = append(chunks, systemcatalog.NewSystemEntity(schemaName, tableName))
	}
	return chunks, nil
}

func encodeKnownTables(
	chunks []systemcatalog.SystemEntity,
) ([]byte, error) {

	buffer := encoding.NewWriteBuffer(1024)

	numOfChunks := len(chunks)
	if err := buffer.PutUint32(uint32(numOfChunks)); err != nil {
		return nil, errors.Wrap(err, 0)
	}

	for i := 0; i < numOfChunks; i++ {
		chunk := chunks[i]
		if err := buffer.PutString(chunk.SchemaName()); err != nil {
			return nil, errors.Wrap(err, 0)
		}
		if err := buffer.PutString(chunk.TableName()); err != nil {
			return nil, errors.Wrap(err, 0)
		}
	}
	return buffer.Bytes(), nil
}

func extractCanonicalNameMapper(
	item systemcatalog.SystemEntity, _ int,
) string {

	return item.CanonicalName()
}
