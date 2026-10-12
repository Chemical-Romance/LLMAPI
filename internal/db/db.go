package db

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"llm-api/internal/domain"
)

// DB wraps the Mongo client and concrete hands on collections.
type DB struct {
	Client *mongo.Client
	Mongo  *mongo.Database

	Users         *mongo.Collection
	Tokens        *mongo.Collection
	Channels      *mongo.Collection
	ChannelKeys   *mongo.Collection
	Quotas        *mongo.Collection
	VirtualModels *mongo.Collection
	RequestLogs   *mongo.Collection
	Settings      *mongo.Collection
}

// Connect connects to MongoDB and initializes indexes.
func Connect(ctx context.Context, uri, dbName string) (*DB, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}
	// v2: ping to verify
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo ping: %w", err)
	}
	d := &DB{
		Client:        client,
		Mongo:         client.Database(dbName),
		Users:         client.Database(dbName).Collection("users"),
		Tokens:        client.Database(dbName).Collection("tokens"),
		Channels:      client.Database(dbName).Collection("channels"),
		ChannelKeys:   client.Database(dbName).Collection("channelKeys"),
		Quotas:        client.Database(dbName).Collection("quotas"),
		VirtualModels: client.Database(dbName).Collection("virtualModels"),
		RequestLogs:   client.Database(dbName).Collection("requestLogs"),
		Settings:      client.Database(dbName).Collection("settings"),
	}
	if err := d.ensureIndexes(ctx); err != nil {
		client.Disconnect(context.Background())
		return nil, err
	}
	return d, nil
}

func (d *DB) ensureIndexes(ctx context.Context) error {
	type idx struct {
		coll *mongo.Collection
		name string
		keys bson.D
		uniq bool
	}
	indexes := []idx{
		{d.Tokens, "uniq_keyHash", bson.D{{Key: "keyHash", Value: 1}}, true},
		{d.Tokens, "user", bson.D{{Key: "userId", Value: 1}}, false},
		{d.ChannelKeys, "channel", bson.D{{Key: "channelId", Value: 1}}, false},
		{d.Quotas, "uniq_channel_model", bson.D{{Key: "channelId", Value: 1}, {Key: "model", Value: 1}}, true},
		{d.VirtualModels, "uniq_name", bson.D{{Key: "name", Value: 1}}, true},
		{d.RequestLogs, "createdAt", bson.D{{Key: "createdAt", Value: -1}}, false},
		{d.RequestLogs, "user_created", bson.D{{Key: "userId", Value: 1}, {Key: "createdAt", Value: -1}}, false},
		{d.Settings, "uniq_key", bson.D{{Key: "key", Value: 1}}, true},
	}
	for _, ix := range indexes {
		opts := options.Index().SetUnique(ix.uniq).SetName(ix.name)
		if _, err := ix.coll.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: ix.keys, Options: opts}); err != nil {
			return fmt.Errorf("create index %s: %w", ix.name, err)
		}
	}
	return nil
}

// GetSetting reads a settings value ("" if absent).
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var s domain.Settings
	err := d.Settings.FindOne(ctx, bson.M{"key": key}).Decode(&s)
	if err == mongo.ErrNoDocuments {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return s.Value, nil
}

// SetSetting upserts a settings value.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.Settings.UpdateOne(
		ctx,
		bson.M{"key": key},
		bson.M{"$set": bson.M{"key": key, "value": value, "updatedAt": time.Now()}},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

// Logf is a small helper to keep startup output consistent.
func Logf(format string, args ...any) {
	log.Printf("[init] "+format, args...)
}
