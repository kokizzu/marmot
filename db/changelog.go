package db

import (
    "bytes"
    "errors"
    "fmt"
    "regexp"
    "strings"
    "text/template"
    "time"

    _ "embed"

    "github.com/doug-martin/goqu/v9"
    "github.com/fsnotify/fsnotify"
    "github.com/rs/zerolog/log"
    "github.com/samber/lo"
)

//go:embed table_change_log_script.tmpl
var tableChangeLogScriptTemplate string
var tableChangeLogTpl *template.Template

var spaceStripper = regexp.MustCompile(`\n\s+`)

type ChangeLogState = int16

const (
    Pending ChangeLogState = 0
    Failed                 = -1
)
const changeLogName = "change_log"
const upsertQuery = `INSERT OR REPLACE INTO %s(%s) VALUES (%s)`

type triggerTemplateData struct {
    Prefix    string
    TableName string
    Columns   []*ColumnInfo
    Triggers  map[string]string
}

type changeLogEntry struct {
    Id    int64  `db:"id"`
    Type  string `db:"type"`
    State string `db:"state"`
}

func init() {
    tableChangeLogTpl = template.Must(
        template.New("tableChangeLogScriptTemplate").Parse(tableChangeLogScriptTemplate),
    )
}

func (conn *SqliteStreamDB) tableCDCScriptFor(tableName string) (string, error) {
    columns, ok := conn.watchTablesSchema[tableName]
    if !ok {
        return "", errors.New("table info not found")
    }

    buf := new(bytes.Buffer)
    err := tableChangeLogTpl.Execute(buf, &triggerTemplateData{
        Prefix:    conn.prefix,
        Triggers:  map[string]string{"insert": "NEW", "update": "NEW", "delete": "OLD"},
        Columns:   columns,
        TableName: tableName,
    })

    if err != nil {
        return "", err
    }

    return spaceStripper.ReplaceAllString(buf.String(), "\n    "), nil
}

func (conn *SqliteStreamDB) Replicate(event *ChangeLogEvent) error {
    if err := conn.consumeReplicationEvent(event); err != nil {
        return err
    }
    return nil
}

func (conn *SqliteStreamDB) consumeReplicationEvent(event *ChangeLogEvent) error {
    return conn.WithTx(func(tnx *goqu.TxDatabase) error {
        primaryKeyMap := conn.getPrimaryKeyMap(event)
        return replicateRow(tnx, event, primaryKeyMap)
    })
}

func (conn *SqliteStreamDB) getPrimaryKeyMap(event *ChangeLogEvent) map[string]any {
    ret := make(map[string]any)
    tableColsSchema, ok := conn.watchTablesSchema[event.TableName]
    if !ok {
        return nil
    }

    for _, col := range tableColsSchema {
        if col.IsPrimaryKey {
            ret[col.Name] = event.Row[col.Name]
        }
    }

    return ret
}

func (conn *SqliteStreamDB) initTriggers(tableNames []string) error {
    for _, tableName := range tableNames {
        name := strings.TrimSpace(tableName)
        if strings.HasPrefix(name, "sqlite_") || strings.HasPrefix(name, conn.prefix) {
            continue
        }

        script, err := conn.tableCDCScriptFor(name)
        if err != nil {
            log.Error().Err(err).Msg("Failed to prepare CDC statement")
            return err
        }

        log.Info().Msg(fmt.Sprintf("Creating trigger for %v", name))
        _, err = conn.Exec(script)
        if err != nil {
            return err
        }
    }

    return nil
}

func (conn *SqliteStreamDB) watchChanges(path string) {
    shmPath := path + "-shm"
    walPath := path + "-wal"
    watcher := conn.watcher

    errShm := watcher.Add(shmPath)
    errWal := watcher.Add(walPath)

    for {
        select {
        case ev, ok := <-conn.watcher.Events:
            if !ok {
                return
            }

            if ev.Op != fsnotify.Chmod {
                conn.publishChangeLog()
            }
        case <-time.After(time.Millisecond * 500):
            conn.publishChangeLog()
            if errShm != nil {
                errShm = watcher.Add(shmPath)
            }

            if errWal != nil {
                errWal = watcher.Add(walPath)
            }
        }
    }
}

func (conn *SqliteStreamDB) publishChangeLog() {
    conn.publishLock.Lock()
    defer conn.publishLock.Unlock()

    scanLimit := uint(100)

    for tableName := range conn.watchTablesSchema {
        var changes []*changeLogEntry
        err := conn.WithTx(func(tx *goqu.TxDatabase) error {
            return tx.Select("id", "type", "state").
                From(conn.metaTable(tableName, changeLogName)).
                Where(goqu.Ex{"state": Pending}).
                Limit(scanLimit).
                Prepared(true).
                ScanStructs(&changes)
        })

        if err != nil {
            log.Error().Err(err).Msg("Error scanning last row ID")
            return
        }

        if len(changes) <= 0 {
            return
        }

        err = conn.consumeChangeLogs(tableName, changes)
        if err != nil {
            log.Error().Err(err).Msg("Unable to consume changes")
        }

        if uint(len(changes)) <= scanLimit {
            break
        }
    }
}

func (conn *SqliteStreamDB) consumeChangeLogs(tableName string, changes []*changeLogEntry) error {
    // Consume each batch in transaction
    deletes, upserts := conn.splitDeleteAndUpserts(changes)

    err := conn.consumeDeletes(tableName, deletes)
    if err != nil {
        return err
    }

    err = conn.consumeUpserts(tableName, upserts)
    if err != nil {
        return err
    }

    return nil
}

func (conn *SqliteStreamDB) consumeUpserts(tableName string, upserts []*changeLogEntry) error {
    rowIds := lo.Map[*changeLogEntry, int64](upserts, func(e *changeLogEntry, i int) int64 {
        return e.Id
    })

    tableCols := conn.watchTablesSchema[tableName]
    columnNames := make([]any, 0)
    idColumnName := conn.prefix + "change_log_id"
    columnNames = append(columnNames, goqu.C("id").As(idColumnName))
    for _, col := range tableCols {
        columnNames = append(columnNames, goqu.C("val_"+col.Name).As(col.Name))
    }

    query, params, err := conn.From(conn.metaTable(tableName, changeLogName)).
        Select(columnNames...).
        Where(goqu.C("id").In(rowIds)).
        Prepared(true).
        ToSQL()
    if err != nil {
        return err
    }

    rawRows, err := conn.Query(query, params...)
    if err != nil {
        return nil
    }

    rows := &enhancedRows{rawRows}
    defer rows.Finalize()

    upsertMap := lo.Associate[*changeLogEntry, int64, *changeLogEntry](
        upserts,
        func(l *changeLogEntry) (int64, *changeLogEntry) {
            return l.Id, l
        },
    )

    for rows.Next() {
        row, err := rows.fetchRow()
        if err != nil {
            return err
        }

        changeRowID := row[idColumnName].(int64)
        changeRow := upsertMap[changeRowID]
        delete(row, idColumnName)

        logger := log.With().
            Int64("rowid", changeRowID).
            Str("table", tableName).
            Str("type", changeRow.Type).
            Logger()

        if conn.OnChange != nil {
            err = conn.OnChange(&ChangeLogEvent{
                Id:        changeRow.Id,
                Type:      changeRow.Type,
                TableName: tableName,
                Row:       row,
            })

            if err != nil {
                logger.Error().Err(err).Msg("Upsert failed to notify on change")
                return err
            }
        }

        _, err = conn.
            Delete(conn.metaTable(tableName, changeLogName)).
            Where(goqu.Ex{"id": changeRow.Id}).
            Prepared(true).
            Executor().
            Exec()

        if err != nil {
            logger.Error().Err(err).Msg("Unable to delete change set row")
            return err
        }

        logger.Debug().Msg("Notified upsert...")
    }

    return nil
}

func (conn *SqliteStreamDB) consumeDeletes(tableName string, deletes []*changeLogEntry) error {
    for _, changeRow := range deletes {
        logger := log.With().
            Str("table", tableName).
            Str("type", changeRow.Type).
            Logger()

        if conn.OnChange != nil {
            err := conn.OnChange(&ChangeLogEvent{
                Row:       nil,
                Type:      "delete",
                TableName: tableName,
            })

            if err != nil {
                logger.Error().Err(err).Msg("Delete failed to notify on change")
                return err
            }

            _, err = conn.
                Delete(conn.metaTable(tableName, changeLogName)).
                Where(goqu.Ex{"id": changeRow.Id}).
                Prepared(true).
                Executor().
                Exec()

            if err != nil {
                logger.Error().Err(err).Msg("Unable to delete change set row")
                return err
            }
        }

        logger.Debug().Msg("Notified delete...")
    }

    return nil
}

func (conn *SqliteStreamDB) splitDeleteAndUpserts(tableChanges []*changeLogEntry) ([]*changeLogEntry, []*changeLogEntry) {
    deletes := make([]*changeLogEntry, 0)
    upserts := make([]*changeLogEntry, 0)
    for _, tableChange := range tableChanges {
        if strings.ToLower(tableChange.Type) == "delete" {
            deletes = append(deletes, tableChange)
        } else {
            upserts = append(upserts, tableChange)
        }
    }
    return deletes, upserts
}

func replicateRow(tx *goqu.TxDatabase, event *ChangeLogEvent, pkMap map[string]any) error {
    if event.Type == "insert" || event.Type == "update" {
        return replicateUpsert(tx, event, pkMap)
    }

    if event.Type == "delete" {
        return replicateDelete(tx, event, pkMap)
    }

    return errors.New(fmt.Sprintf("invalid operation type %s", event.Type))
}

func replicateUpsert(tx *goqu.TxDatabase, event *ChangeLogEvent, _ map[string]any) error {
    columnNames := make([]string, 0, len(event.Row))
    columnValues := make([]any, 0, len(event.Row))
    for k, v := range event.Row {
        columnNames = append(columnNames, k)
        columnValues = append(columnValues, v)
    }

    query := fmt.Sprintf(
        upsertQuery,
        event.TableName,
        strings.Join(columnNames, ", "),
        strings.Join(strings.Split(strings.Repeat("?", len(columnNames)), ""), ", "),
    )

    stmt, err := tx.Prepare(query)
    if err != nil {
        return err
    }

    _, err = stmt.Exec(columnValues...)
    return err
}

func replicateDelete(tx *goqu.TxDatabase, event *ChangeLogEvent, pkMap map[string]any) error {
    _, err := tx.Delete(event.TableName).
        Where(goqu.Ex(pkMap)).
        Prepared(true).
        Executor().
        Exec()

    return err
}
