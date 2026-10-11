// Command migrate creates the tables and seed data on the database in DATABASE_URL.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"sutra/internal/config"
	"sutra/internal/db"
)

func main() {
	config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := db.Connect(ctx, config.MustString("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer pool.Close()

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(applied) == 0 {
		fmt.Println("database is up to date")
		return
	}
	for _, v := range applied {
		fmt.Println("applied", v)
	}
}
