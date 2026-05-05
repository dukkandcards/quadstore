// Partitioned-store support. A partitioned Store backs N independent
// SQLite files behind one Reader / Writer / Batch surface; routing is
// consumer-supplied. Designed for consumers whose data has clear
// non-overlapping query families (e.g., a no-action-letter analytical
// database whose comment-letter corpus shares no queries with the
// no-action data). See docs/PARTITIONING_DESIGN.md.

package quadstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Partition is an opaque routing key. The empty Partition routes to a
// PartitionedConfig.Default. A single-file Store has one partition with
// name "" — legacy callers see exactly that shape.
type Partition string

// LabelRouter maps a label to a partition. Receives the effective label
// of a write (Quad.Label || Batch.Label) and returns the partition the
// write must land on. Returning the empty Partition routes to the
// configured Default. Required for OpenPartitioned.
//
// Routers are deterministic and stateless: the same input must always
// produce the same output.
type LabelRouter func(label string) Partition

// PatternRouter maps a Pattern to a partition for read optimization.
// Receives the full Pattern at every Reader.Find / Reader.Count and
// returns the partition the query can be scoped to, or "" to indicate
// the library should fall back to label-based routing or fan out.
//
// Optional. When nil, the library scopes a query to one partition only
// when Pattern.Label resolves via LabelRouter; otherwise it fans out.
//
// The PatternRouter is the place to encode optimization beyond label —
// e.g. "subject prefix `cmt:` always lives in partition `corpus`."
// The library never guesses; consumers encode their routing knowledge here.
type PatternRouter func(p Pattern) Partition

// PartitionSpec names a partition and its backing file path.
type PartitionSpec struct {
	Name Partition // unique within a PartitionedConfig; "" reserved for default sentinel
	File string   // path; if relative, taken relative to PartitionedConfig.Root
}

// PartitionedConfig configures a partitioned Store.
type PartitionedConfig struct {
	// Root is the directory used to resolve relative PartitionSpec.File
	// paths. Optional — absolute File paths bypass it. If both Root and
	// File are empty for a spec, OpenPartitioned errors.
	Root string

	// Partitions enumerates the partitions and their backing files. The
	// order is significant only as the iteration order for fan-out reads
	// and admin commands; logical equivalence does not depend on order.
	// Must contain at least one entry. Names must be unique and non-empty.
	Partitions []PartitionSpec

	// Default is the partition that receives writes whose RouteLabel
	// returns "". Must match one of Partitions[*].Name.
	Default Partition

	// RouteLabel routes a write to a partition by label. Required.
	RouteLabel LabelRouter

	// RoutePattern optionally routes a read to a partition. May be nil.
	RoutePattern PatternRouter
}

// Sentinel errors returned by partitioned-Store operations. All are
// testable from outside the package via errors.Is.
var (
	// ErrCrossPartitionBatch is returned by Writer.Commit when a single
	// Batch contains quads that route to more than one partition.
	// Callers split the batch by partition and commit each separately.
	ErrCrossPartitionBatch = errors.New("quadstore: batch crosses partitions")

	// ErrUnknownPartition is returned when a Partition name is not
	// present in the Store's configuration.
	ErrUnknownPartition = errors.New("quadstore: unknown partition")

	// ErrUnroutableLabel is returned when a label cannot be routed to
	// any partition: RouteLabel returned an unknown name and no Default
	// is configured for fallback.
	ErrUnroutableLabel = errors.New("quadstore: label cannot be routed")

	// ErrNoPartitions is returned by OpenPartitioned when the supplied
	// PartitionedConfig has no Partitions.
	ErrNoPartitions = errors.New("quadstore: no partitions configured")

	// ErrDuplicatePartition is returned when PartitionedConfig.Partitions
	// contains two specs with the same Name.
	ErrDuplicatePartition = errors.New("quadstore: duplicate partition name")

	// ErrEmptyPartitionName is returned when a PartitionSpec has Name == "".
	// The empty name is reserved for the single-file Store sentinel.
	ErrEmptyPartitionName = errors.New("quadstore: partition name must be non-empty")

	// ErrMissingDefault is returned when PartitionedConfig.Default is "" or
	// does not match any partition's Name.
	ErrMissingDefault = errors.New("quadstore: default partition not in Partitions")

	// ErrMissingRouter is returned when PartitionedConfig.RouteLabel is nil.
	ErrMissingRouter = errors.New("quadstore: RouteLabel is required")
)

// partitionConn is the per-partition backing connection. Internal.
type partitionConn struct {
	name       Partition
	db         *sql.DB
	writerSlot chan struct{} // single-writer per file, SQLite hard constraint
}

// OpenPartitioned opens or creates a partitioned Store. Each partition
// is a fully independent SQLite file with the standard schema; reads
// fan out across partitions when not scoped, writes route by label.
//
// API-compatibility: the returned *Store is the same type as one returned
// by Open. Existing callers using Reader / Writer / Batch / Add /
// AddBatch / Delete / Vacuum / Stats work unchanged; the routing happens
// inside the library.
//
// Each partition file is opened with the standard PRAGMAs: WAL,
// busy_timeout=5s, synchronous=NORMAL, cache_size=256 MB,
// journal_size_limit=500 MB. Total file-handle cost: 1 connection +
// 1 WAL + 1 SHM per partition.
func OpenPartitioned(cfg PartitionedConfig) (*Store, error) {
	if len(cfg.Partitions) == 0 {
		return nil, ErrNoPartitions
	}
	if cfg.RouteLabel == nil {
		return nil, ErrMissingRouter
	}

	parts := make([]*partitionConn, 0, len(cfg.Partitions))
	byName := make(map[Partition]*partitionConn, len(cfg.Partitions))
	for _, spec := range cfg.Partitions {
		if spec.Name == "" {
			closeAll(parts)
			return nil, ErrEmptyPartitionName
		}
		if _, dup := byName[spec.Name]; dup {
			closeAll(parts)
			return nil, fmt.Errorf("%w: %s", ErrDuplicatePartition, spec.Name)
		}
		path := spec.File
		if !filepath.IsAbs(path) && cfg.Root != "" {
			path = filepath.Join(cfg.Root, path)
		}
		db, err := openSQLite(path)
		if err != nil {
			closeAll(parts)
			return nil, fmt.Errorf("quadstore: open partition %s: %w", spec.Name, err)
		}
		if err := migrate(db); err != nil {
			db.Close()
			closeAll(parts)
			return nil, fmt.Errorf("quadstore: migrate partition %s: %w", spec.Name, err)
		}
		conn := &partitionConn{
			name:       spec.Name,
			db:         db,
			writerSlot: make(chan struct{}, 1),
		}
		parts = append(parts, conn)
		byName[spec.Name] = conn
	}

	if cfg.Default == "" {
		closeAll(parts)
		return nil, ErrMissingDefault
	}
	if _, ok := byName[cfg.Default]; !ok {
		closeAll(parts)
		return nil, fmt.Errorf("%w: %s", ErrMissingDefault, cfg.Default)
	}

	return &Store{
		parts:        parts,
		byName:       byName,
		defaultName:  cfg.Default,
		routeLabel:   cfg.RouteLabel,
		routePattern: cfg.RoutePattern,
	}, nil
}

// openSQLite is the shared connection-open path used by both Open and
// OpenPartitioned. Centralizing it keeps the PRAGMA set in one place.
func openSQLite(path string) (*sql.DB, error) {
	dsn := path + "?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=cache_size(-262144)" +
		"&_pragma=journal_size_limit(500000000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// closeAll closes every partition connection. Used for OpenPartitioned
// failure cleanup. Errors are dropped (best-effort cleanup path).
func closeAll(parts []*partitionConn) {
	for _, p := range parts {
		_ = p.db.Close()
	}
}

// partFor resolves a label to its partition connection. Returns the
// default partition when RouteLabel returns "" or returns a name unknown
// to the Store (lenient — unknown names route to default rather than
// failing, matching SQLite's permissive nature).
func (s *Store) partFor(label string) *partitionConn {
	if s.routeLabel == nil {
		// Single-file Store: one partition, return it.
		return s.parts[0]
	}
	target := s.routeLabel(label)
	if target == "" {
		return s.byName[s.defaultName]
	}
	if conn, ok := s.byName[target]; ok {
		return conn
	}
	return s.byName[s.defaultName]
}

// partForPattern resolves a Pattern to its partition or returns nil
// if the read must fan out. Used by Reader.
//
// Optimization order (per design doc, "don't guess, optimize"):
//  1. RoutePattern, if configured — consumer-supplied logic, can use any
//     Pattern field. Returning a non-empty known Partition scopes the read.
//  2. If Pattern.Label != "", call RouteLabel. Returning a non-empty
//     known Partition scopes the read.
//  3. Otherwise nil — fan out across all partitions.
//
// Single-file stores: always return the lone partition.
func (s *Store) partForPattern(p Pattern) *partitionConn {
	if !s.partitioned() {
		return s.parts[0]
	}
	if s.routePattern != nil {
		if name := s.routePattern(p); name != "" {
			if conn, ok := s.byName[name]; ok {
				return conn
			}
		}
	}
	if p.Label != "" {
		if name := s.routeLabel(p.Label); name != "" {
			if conn, ok := s.byName[name]; ok {
				return conn
			}
		}
	}
	return nil
}

// partitioned reports whether this Store has more than one partition.
// Used to short-circuit fan-out logic on legacy single-file stores.
func (s *Store) partitioned() bool {
	return len(s.parts) > 1
}

// PartitionFor returns the partition name that a write with the given
// label would route to. Useful for consumers verifying routing decisions
// outside the Writer path. On single-file Stores, always returns "".
func (s *Store) PartitionFor(label string) Partition {
	return s.partFor(label).name
}

// Partitions returns the set of partition names configured on this
// Store, in the order supplied at OpenPartitioned. A single-file Store
// returns a single-element slice containing "".
func (s *Store) Partitions() []Partition {
	out := make([]Partition, 0, len(s.parts))
	for _, p := range s.parts {
		out = append(out, p.name)
	}
	return out
}

// VacuumFor runs VACUUM on a single partition, identified by name.
// Returns ErrUnknownPartition if name is not configured.
func (s *Store) VacuumFor(ctx context.Context, name Partition) error {
	conn, ok := s.byName[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownPartition, name)
	}
	if _, err := conn.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("quadstore: vacuum %s: %w", name, err)
	}
	return nil
}
