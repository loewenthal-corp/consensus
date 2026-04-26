package postgres

import (
	"database/sql"

	"entgo.io/ent/dialect"
)

type sqlDBProvider interface {
	DB() *sql.DB
}

func (c *Client) SQLDB() *sql.DB {
	if c == nil || c.driver == nil {
		return nil
	}
	if provider, ok := c.driver.(sqlDBProvider); ok {
		return provider.DB()
	}
	if debug, ok := c.driver.(*dialect.DebugDriver); ok {
		if provider, ok := debug.Driver.(sqlDBProvider); ok {
			return provider.DB()
		}
	}
	return nil
}
