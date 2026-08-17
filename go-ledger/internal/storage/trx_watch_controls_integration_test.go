package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPostgresTRXWatchControlsMigrationDisablesLegacyTRXNotifications(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, admin, schema := postgresTestSchema(t, ctx, dsn, "trx_watch_controls")

	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	if _, err := admin.Exec(ctx, `DELETE FROM `+schema+`.schema_migrations WHERE version=$1`, trxWatchControlsMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.address_watches(owner_user_id,address,notify_trx,min_notify_trx_amount,created_at,updated_at)
		VALUES(1,'TLegacy',TRUE,'2.5',NOW(),NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.address_watch_settings(owner_user_id,notify_trx,min_notify_trx_amount,updated_at)
		VALUES(1,TRUE,'3.5',NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.chain_watcher_bots(bot_id,secret,created_at,updated_at)
		VALUES('bot','secret',NOW(),NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.chain_watcher_subscriptions(bot_id,chat_id,owner_user_id,address,notify_trx,min_notify_trx_amount,created_at,updated_at)
		VALUES('bot',1,1,'TLegacy',TRUE,'4.5',NOW(),NOW())`); err != nil {
		t.Fatal(err)
	}

	store, err = Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	var watchTRX, settingsTRX, subscriptionTRX bool
	var watchMin, settingsMin, subscriptionMin string
	if err := admin.QueryRow(ctx, `SELECT
		(SELECT notify_trx FROM `+schema+`.address_watches WHERE owner_user_id=1 AND address='TLegacy'),
		(SELECT min_notify_trx_amount FROM `+schema+`.address_watches WHERE owner_user_id=1 AND address='TLegacy'),
		(SELECT notify_trx FROM `+schema+`.address_watch_settings WHERE owner_user_id=1),
		(SELECT min_notify_trx_amount FROM `+schema+`.address_watch_settings WHERE owner_user_id=1),
		(SELECT notify_trx FROM `+schema+`.chain_watcher_subscriptions WHERE bot_id='bot' AND owner_user_id=1),
		(SELECT min_notify_trx_amount FROM `+schema+`.chain_watcher_subscriptions WHERE bot_id='bot' AND owner_user_id=1)`).Scan(
		&watchTRX, &watchMin, &settingsTRX, &settingsMin, &subscriptionTRX, &subscriptionMin,
	); err != nil {
		t.Fatal(err)
	}
	if watchTRX || settingsTRX || subscriptionTRX {
		t.Fatalf("legacy TRX notification flags were not disabled: watch=%t settings=%t subscription=%t", watchTRX, settingsTRX, subscriptionTRX)
	}
	if watchMin != "2.5" || settingsMin != "3.5" || subscriptionMin != "4.5" {
		t.Fatalf("TRX thresholds changed unexpectedly: watch=%q settings=%q subscription=%q", watchMin, settingsMin, subscriptionMin)
	}
}
