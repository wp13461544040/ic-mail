package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Enabled-state revisions are independent of access timestamps, labels and
// credential rotation. Triggers cover every existing alias-enable writer.
func (s *Store) migrateAliasReceiveRevisions(ctx context.Context, tx *sql.Tx) error {
	statements := []string{`CREATE TABLE IF NOT EXISTS alias_receive_revisions (
		alias_id BIGINT PRIMARY KEY REFERENCES aliases(id) ON DELETE CASCADE,
		revision BIGINT NOT NULL CHECK(revision > 0)
	)`}
	if s.dialect == dialectPostgres {
		statements = append(statements, `CREATE OR REPLACE FUNCTION icloud_alias_receive_revision() RETURNS trigger AS $$
		BEGIN
			INSERT INTO alias_receive_revisions(alias_id, revision) VALUES(NEW.id, 1)
			ON CONFLICT(alias_id) DO UPDATE SET revision = alias_receive_revisions.revision + 1;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`,
			`DROP TRIGGER IF EXISTS alias_receive_revision_update ON aliases`,
			`CREATE TRIGGER alias_receive_revision_update AFTER UPDATE OF enabled ON aliases
			FOR EACH ROW WHEN (OLD.enabled IS DISTINCT FROM NEW.enabled)
			EXECUTE FUNCTION icloud_alias_receive_revision()`)
	} else {
		statements = append(statements, `CREATE TRIGGER IF NOT EXISTS alias_receive_revision_update
		AFTER UPDATE OF enabled ON aliases WHEN OLD.enabled <> NEW.enabled
		BEGIN
			INSERT INTO alias_receive_revisions(alias_id, revision) VALUES(NEW.id, 1)
			ON CONFLICT(alias_id) DO UPDATE SET revision = alias_receive_revisions.revision + 1;
		END`)
	}
	for _, statement := range statements {
		if _, err := s.txExecContext(ctx, tx, statement); err != nil {
			return fmt.Errorf("migrate alias receive revisions: %w", err)
		}
	}
	return nil
}
