package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"

	// pgx を database/sql ドライバとして登録する(#228)。
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgreSQL 版のデータブラウザ(#228)。クエリ実行そのものは engine 非依存
// (database/sql の generic scan)なので、engine で変わるのは接続文字列と
// スキーマ問い合わせの 2 つだけ。
//
// MySQL と違い PostgreSQL の接続は 1 つのデータベースに束縛され、接続を跨いだ
// 問い合わせができない。そこで「アプリの DB」を 1 つ選んで、その中のスキーマを
// 見せる(既定 DB の postgres / template* は避ける)。baseline import --db で
// 作った DB が普通は 1 個なので、実用上はこれで目的の DB に当たる。

// pgDSN は指定 DB への接続 URL を組む。資格情報に記号が入っても壊れないよう
// url.UserPassword でエスケープする。
func pgDSN(user, pass string, port int, dbname string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, pass),
		Host:   "127.0.0.1:" + strconv.Itoa(port),
		Path:   "/" + dbname,
	}
	q := u.Query()
	// proxy を介さない直接接続(loopback)。TLS は張らない。
	q.Set("sslmode", "disable")
	q.Set("connect_timeout", "10")
	u.RawQuery = q.Encode()
	return u.String()
}

// pgOpen は指定 DB への *sql.DB を返す。
func pgOpen(user, pass string, port int, dbname string) (*sql.DB, error) {
	db, err := sql.Open("pgx", pgDSN(user, pass, port, dbname))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

// pgBrowseDatabase は「見せる DB」を選ぶ。テンプレートと管理用 DB を除いた
// 最初のものを使い、無ければ postgres に落とす。
func pgBrowseDatabase(ctx context.Context, user, pass string, port int) (string, error) {
	admin, err := pgOpen(user, pass, port, "postgres")
	if err != nil {
		return "", err
	}
	defer func() { _ = admin.Close() }()

	rows, err := admin.QueryContext(ctx, `
		SELECT datname FROM pg_database
		WHERE datistemplate = false AND datallowconn = true AND datname <> 'postgres'
		ORDER BY datname`)
	if err != nil {
		return "", fmt.Errorf("list databases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		return name, nil // 最初のアプリ DB
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "postgres", nil
}

// pgSchemaQuery は MySQL 版と同じ列(schema, table, 概算行数, column, type,
// nullable, key)を返す PostgreSQL 用のクエリ。
//
// MySQL の information_schema にある column_type / column_key / table_rows は
// PostgreSQL に無いので、それぞれ data_type / 主キー制約からの導出 /
// pg_class.reltuples で埋める(reltuples は ANALYZE 時点の概算で、MySQL の
// table_rows も概算なので性質は揃っている)。
const pgSchemaQuery = `
	SELECT c.table_schema,
	       c.table_name,
	       COALESCE(cl.reltuples, 0)::bigint AS approx_rows,
	       c.column_name,
	       c.data_type,
	       c.is_nullable,
	       CASE WHEN pk.column_name IS NOT NULL THEN 'PRI' ELSE '' END AS column_key
	FROM information_schema.columns c
	JOIN information_schema.tables t
	  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
	LEFT JOIN pg_catalog.pg_namespace ns
	  ON ns.nspname = c.table_schema
	LEFT JOIN pg_catalog.pg_class cl
	  ON cl.relname = c.table_name AND cl.relnamespace = ns.oid
	LEFT JOIN (
	  SELECT kcu.table_schema, kcu.table_name, kcu.column_name
	  FROM information_schema.table_constraints tc
	  JOIN information_schema.key_column_usage kcu
	    ON kcu.constraint_name = tc.constraint_name
	   AND kcu.table_schema = tc.table_schema
	  WHERE tc.constraint_type = 'PRIMARY KEY'
	) pk
	  ON pk.table_schema = c.table_schema
	 AND pk.table_name = c.table_name
	 AND pk.column_name = c.column_name
	WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
	  AND t.table_type = 'BASE TABLE'
	ORDER BY c.table_schema, c.table_name, c.ordinal_position`
