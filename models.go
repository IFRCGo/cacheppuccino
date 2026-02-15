package main

import (
	"time"

	"github.com/uptrace/bun"
)

type StringModel struct {
	bun.BaseModel `bun:"table:strings"`

	Page      string    `bun:"page,pk,notnull"`
	Key       string    `bun:"key,pk,notnull"`
	Lang      string    `bun:"lang,pk,notnull"`
	Value     string    `bun:"value,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}

type MetaModel struct {
	bun.BaseModel `bun:"table:meta"`

	K string `bun:"k,pk,notnull"`
	V string `bun:"v,notnull"`
}
