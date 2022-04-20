package migrations

import (
	"fmt"

	"github.com/google/uuid"
)

func createMetadataTable(driver *MySQLDriver) string {
	return fmt.Sprintf(`CREATE TABLE %s (
		unique_id VARCHAR(36) PRIMARY KEY);`,
		driver.Metadata(),
	)
}

func insertUniqueID(driver *MySQLDriver) string {
	return fmt.Sprintf(`INSERT INTO %s (unique_id) VALUES ("%s");`,
		driver.Metadata(),
		uuid.NewString(),
	)
}

func init() {
	mustRegisterMigration("add_unique_datastore_id", "initial",
		newExecutor(
			createMetadataTable,
			insertUniqueID,
		).migrate,
	)
}
