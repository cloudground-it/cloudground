/** Quarry: the shapes of /api/quarry/… (internal/api/studio.go). */

export interface DBStudioTable {
  name: string
  type: 'table' | 'view'
  engine: string
  collation: string
  rows: number
  data_bytes: number
  index_bytes: number
  auto_increment?: number
  comment?: string
}

export interface DBRoutine {
  name: string
  type: 'procedure' | 'function'
}

export interface DBStudioTables {
  database: string
  tables: DBStudioTable[]
  /** Stored procedures and functions: listed, never run or changed. */
  routines?: DBRoutine[]
  /** Table → its columns, when asked for with ?columns=1. */
  columns?: Record<string, string[]>
}

export interface DBColumn {
  name: string
  type: string
  data_type: string
  nullable: boolean
  default: string | null
  key?: string
  extra?: string
  collation: string
  comment: string
  binary?: boolean
}

export interface DBIndexColumn {
  name: string
  length?: number
}

export interface DBIndex {
  name: string
  unique: boolean
  type: string
  columns: DBIndexColumn[]
}

export interface DBForeignKey {
  name: string
  columns: string[]
  ref_table: string
  ref_columns: string[]
  on_update: string
  on_delete: string
}

export interface DBStructure {
  table: string
  type: string
  columns: DBColumn[]
  indexes: DBIndex[]
  foreign_keys: DBForeignKey[]
  primary_key: string[]
  create_sql: string
}

export type DBFilterOp = 'eq' | 'ne' | 'lt' | 'le' | 'gt' | 'ge' | 'contains' | 'starts' | 'null' | 'notnull'

export interface DBSort { column: string, desc: boolean }
export interface DBFilter { column: string, op: DBFilterOp, value: string }

export interface DBRow {
  v: (string | null)[]
  /** Indexes of the cells whose value was cut for display. */
  cut?: number[]
}

export interface DBRows {
  columns: DBColumn[]
  primary_key: string[]
  rows: DBRow[]
  total: number
  total_estimated?: boolean
  page: number
  page_size: number
  sql: string
}

export interface DBChange {
  op: 'insert' | 'update' | 'delete'
  key?: Record<string, string>
  values?: Record<string, string | null>
}

export interface DBEditResult {
  sql: string[]
  affected: number
  executed: boolean
}

export interface DBNewColumn {
  name: string
  type: string
  length: number
  scale: number
  unsigned: boolean
  nullable: boolean
  default: string | null
  after: string
}

export interface DBNewIndex {
  name: string
  unique: boolean
  columns: DBIndexColumn[]
}

export interface DBAlterResult {
  sql: string
  executed: boolean
}

export interface DBQueryResult {
  columns: string[] | null
  rows: string[][] | null
  message?: string
  truncated?: boolean
  read_only?: boolean
}

/** A write statement run in a transaction that is always rolled back. */
export interface DBSimulateResult {
  kind?: 'update' | 'delete' | 'insert' | 'replace'
  /** The rows it matched. */
  affected: number
  /** Up to 25 of the rows an UPDATE or DELETE would change, as they are now. */
  preview?: DBQueryResult
  preview_sql?: string
  /** Why it was not run. */
  refused?: string
  /** The server's error. */
  message?: string
  rolled_back: boolean
}

export interface DBSearchResult {
  tables: { table: string, matches: number }[]
  searched: number
  truncated?: boolean
}

export interface DBStudioFile {
  name: string
  size: number
  modified: string
}

export interface DBStudioFiles {
  exports: DBStudioFile[]
  imports: DBStudioFile[]
}

export interface WPAutoload {
  table: string
  count: number
  bytes: number
  top: { name: string, bytes: number, autoload: string }[]
}

export interface WPSearchReplaceResult {
  replacements: number
  executed: boolean
}

export interface WPTransientsResult {
  expired: number
  deleted: number
  executed: boolean
}

/** A site's database, as GET /api/sites/{id}/databases lists it. */
export interface SiteDatabase {
  name: string
  /** The external server it is on; 0 is this server. */
  server_id: number
  /** The database the site's configuration (wp-config.php) uses. */
  primary: boolean
  /** -1 when the agent could not read it. */
  size_bytes: number
  tables: number
  created_at: string
}

export type DBGrantLevel = 'all' | 'read'
export interface DBGrant { database: string, level: DBGrantLevel }

/** A site's database user and its privileges. */
export interface SiteDBUser {
  name: string
  server_id: number
  primary: boolean
  grants: DBGrant[] | null
  created_at: string
}

/** Where an application connects (id 0: this server). */
export interface SiteDBServerRef { id: number, kind: 'mariadb' | 'postgres', name: string, host: string, port: number, tls: 'required' | 'off' }

export interface SiteDatabases {
  databases: SiteDatabase[] | null
  users: SiteDBUser[] | null
  servers?: SiteDBServerRef[]
  available_servers?: { id: number, name: string, kind: 'mariadb' | 'postgres' }[]
  stats_error?: string
}
