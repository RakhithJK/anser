package anser

import (
	"github.com/mongodb/amboy/dependency"
	"github.com/mongodb/amboy/registry"
	"github.com/mongodb/grip"
	"github.com/pkg/errors"
	mgo "gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

func init() {
	registry.AddDependencyType("anser-migration", func() dependency.Manager {
		return makeMigrationDependencyManager()
	})
}

// NewMigrationDependencyManager constructs a new
// amboy/dependency.Manager object, which allows migrations to express
// dependencies to other migrations. The State() method ensures that
// all migration IDs specified as edges are satisfied before reporting
// as "ready" for work.
func NewMigrationDependencyManager(e Environment, migrationID string,
	query map[string]interface{}, ns Namespace) (dependency.Manager, error) {

	d := makeMigrationDependencyManager()
	if err := d.SetEnv(e); err != nil {
		return nil, errors.Wrap(err, "problem with environment")
	}
	d.Query = query
	d.MigrationID = migrationID

	return d, nil
}

type migrationDependency struct {
	MigrationID string                 `bson:"migration" json:"migration" yaml:"migration"`
	Query       map[string]interface{} `bson:"query" json:"query" yaml:"query"`
	NS          Namespace              `bson:"namespace" json:"namespace" yaml:"namespace"`
	T           dependency.TypeInfo    `bson:"type" json:"type" yaml:"type"`

	MigrationHelper `bson:"-" json:"-" yaml:"-"`
	*dependency.JobEdges
}

func makeMigrationDependencyManager() *migrationDependency {
	return &migrationDependency{
		JobEdges: dependency.NewJobEdges(),
		T: dependency.TypeInfo{
			Name:    "anser-migration",
			Version: 0,
		},
	}
}

func (d *migrationDependency) Type() dependency.TypeInfo {
	return d.T
}

func (d *migrationDependency) State() dependency.State {
	switch num := d.PendingMigrationOperations(d.NS, d.Query); num {
	case -1:
		return dependency.Unresolved
	case 0:
		return dependency.Passed
	}

	edges := d.Edges()
	if len(edges) == 0 {
		return dependency.Ready
	}

	// query the "done" dependencies, and make sure that all the
	// edges listed in the edges document are satisfied.

	query := getDependencyStateQuery(edges)
	session, iter, err := d.GetMigrationEvents(query)
	if err != nil {
		grip.Warning(err)
		return dependency.Blocked
	}
	defer session.Close()

	return processEdges(len(edges), iter)
}

func processEdges(numEdges int, iter DocumentIterator) dependency.State {
	count := 0
	meta := &MigrationMetadata{}
	for iter.Next(meta) {
		// if any of the edges are *not* satisfied, then the
		// dependency is by definition blocked
		if !meta.Satisfied() {
			return dependency.Blocked
		}
		count++
	}

	// if we encountered an error in the query, then we should log
	// the error but not let the current task get dispatched.
	if err := iter.Close(); err != nil {
		grip.Warning(err)
		return dependency.Blocked
	}

	// if there are more edges defined than observed in the query,
	// then some tasks haven't reported in or been rejistered, and
	// we're blocked or stuck
	if count < numEdges {
		return dependency.Blocked
	}

	// otherwise, the task is ready for work:
	return dependency.Ready
}

func getDependencyEdgeIter(coll *mgo.Collection, query bson.M) DocumentIterator {
	return coll.Find(query).Iter()
}

func getDependencyStateQuery(ids []string) bson.M {
	orQuery := make([]bson.M, len(ids))

	for idx, id := range ids {
		orQuery[idx] = bson.M{"_id": id}
	}

	return bson.M{"$or": orQuery}
}