package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"linkedin-post-manager/internal/domain"
	"linkedin-post-manager/prompts"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type Store struct {
	client   *mongo.Client
	db       *mongo.Database
	bucket   *mongo.GridFSBucket
	subjects *mongo.Collection
	ideas    *mongo.Collection
	posts    *mongo.Collection
	settings *mongo.Collection
	locks    *mongo.Collection
}

func Connect(ctx context.Context, uri, dbName, defaultWebhookURL string) (*Store, error) {
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, fmt.Errorf("mongo ping: %w", err)
	}

	db := client.Database(dbName)
	s := &Store{
		client:   client,
		db:       db,
		bucket:   db.GridFSBucket(),
		subjects: db.Collection("subjects"),
		ideas:    db.Collection("ideas"),
		posts:    db.Collection("posts"),
		settings: db.Collection("settings"),
		locks:    db.Collection("weekly_locks"),
	}

	if err := s.ensureIndexes(ctx); err != nil {
		return nil, err
	}
	if err := s.seedDefaultSettings(ctx, defaultWebhookURL); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

func (s *Store) ensureIndexes(ctx context.Context) error {
	_, err := s.posts.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "week_key", Value: 1}, {Key: "slot_index", Value: 1}}},
		{Keys: bson.D{{Key: "subject_id", Value: 1}, {Key: "created_at", Value: -1}}},
		{Keys: bson.D{{Key: "is_golden", Value: 1}}},
	})
	if err != nil {
		return fmt.Errorf("create posts indexes: %w", err)
	}

	_, err = s.subjects.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "active", Value: 1}, {Key: "priority", Value: -1}},
	})
	if err != nil {
		return fmt.Errorf("create subjects index: %w", err)
	}

	_, err = s.ideas.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "used", Value: 1}, {Key: "created_at", Value: 1}},
	})
	if err != nil {
		return fmt.Errorf("create ideas index: %w", err)
	}
	return nil
}

func (s *Store) seedDefaultSettings(ctx context.Context, defaultWebhookURL string) error {
	var existing domain.Settings
	err := s.settings.FindOne(ctx, bson.M{"_id": "global"}).Decode(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("check settings: %w", err)
	}

	initial := domain.Settings{
		ID:                 "global",
		PostSkillPrompt:    prompts.DefaultPostSkillPrompt,
		ImageSkillPrompt:   prompts.DefaultImageSkillPrompt,
		AuthorPersona:      "Pragmatic software engineer sharing hands-on architectural lessons, performance trade-offs, and recent ecosystem updates.",
		PreferredDays:      []string{"Tuesday", "Wednesday", "Thursday"},
		DefaultAspectRatio: "4:5",
		WebhookURL:         defaultWebhookURL,
		UpdatedAt:          time.Now().UTC(),
	}
	_, err = s.settings.InsertOne(ctx, initial)
	if mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}

// --- Atomic Weekly Lock ---

func (s *Store) TryAcquireWeekLock(ctx context.Context, weekKey, owner string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	until := now.Add(ttl)

	filter := bson.M{
		"_id": weekKey,
		"$or": []bson.M{
			{"locked_until": bson.M{"$lte": now}},
			{"owner": owner},
		},
	}
	update := bson.M{
		"$set": bson.M{
			"locked_until": until,
			"owner":        owner,
			"updated_at":   now,
		},
	}

	res, err := s.locks.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, err
	}
	if res.MatchedCount > 0 {
		return true, nil
	}

	_, err = s.locks.InsertOne(ctx, bson.M{
		"_id":          weekKey,
		"locked_until": until,
		"owner":        owner,
		"updated_at":   now,
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) ReleaseWeekLock(ctx context.Context, weekKey, owner string) {
	_, _ = s.locks.DeleteOne(ctx, bson.M{"_id": weekKey, "owner": owner})
}

// --- Settings ---

func (s *Store) GetSettings(ctx context.Context) (domain.Settings, error) {
	var cfg domain.Settings
	err := s.settings.FindOne(ctx, bson.M{"_id": "global"}).Decode(&cfg)
	return cfg, err
}

func (s *Store) SaveSettings(ctx context.Context, cfg domain.Settings) error {
	cfg.ID = "global"
	cfg.UpdatedAt = time.Now().UTC()
	_, err := s.settings.ReplaceOne(ctx, bson.M{"_id": "global"}, cfg, options.Replace().SetUpsert(true))
	return err
}

// --- Subjects ---

func (s *Store) ListSubjects(ctx context.Context) ([]domain.Subject, error) {
	opts := options.Find().SetSort(bson.D{{Key: "active", Value: -1}, {Key: "priority", Value: -1}, {Key: "name", Value: 1}})
	cur, err := s.subjects.Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var list []domain.Subject
	if err := cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) ListActiveSubjects(ctx context.Context) ([]domain.Subject, error) {
	opts := options.Find().SetSort(bson.D{{Key: "priority", Value: -1}, {Key: "name", Value: 1}})
	cur, err := s.subjects.Find(ctx, bson.M{"active": true}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var list []domain.Subject
	if err := cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) GetSubject(ctx context.Context, id bson.ObjectID) (domain.Subject, error) {
	var sub domain.Subject
	err := s.subjects.FindOne(ctx, bson.M{"_id": id}).Decode(&sub)
	return sub, err
}

func (s *Store) CreateSubject(ctx context.Context, name, description string, priority int) (domain.Subject, error) {
	if priority < 1 {
		priority = 1
	}
	if priority > 10 {
		priority = 10
	}
	now := time.Now().UTC()
	sub := domain.Subject{
		ID:            bson.NewObjectID(),
		Name:          name,
		Description:   description,
		Priority:      priority,
		CurrentWeight: 0,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	_, err := s.subjects.InsertOne(ctx, sub)
	return sub, err
}

func (s *Store) UpdateSubject(ctx context.Context, id bson.ObjectID, name, description string, priority int, active bool) error {
	if priority < 1 {
		priority = 1
	}
	if priority > 10 {
		priority = 10
	}
	_, err := s.subjects.UpdateByID(ctx, id, bson.M{
		"$set": bson.M{
			"name":        name,
			"description": description,
			"priority":    priority,
			"active":      active,
			"updated_at":  time.Now().UTC(),
		},
	})
	return err
}

func (s *Store) ToggleSubjectActive(ctx context.Context, id bson.ObjectID) (domain.Subject, error) {
	sub, err := s.GetSubject(ctx, id)
	if err != nil {
		return sub, err
	}
	sub.Active = !sub.Active
	sub.UpdatedAt = time.Now().UTC()
	_, err = s.subjects.UpdateByID(ctx, id, bson.M{
		"$set": bson.M{"active": sub.Active, "updated_at": sub.UpdatedAt},
	})
	return sub, err
}

func (s *Store) SaveSubjectWeightsAndUsage(ctx context.Context, updated []domain.Subject, chosenID bson.ObjectID) error {
	now := time.Now().UTC()
	for _, sub := range updated {
		setFields := bson.M{
			"current_weight": sub.CurrentWeight,
			"updated_at":     now,
		}
		update := bson.M{"$set": setFields}
		if sub.ID == chosenID {
			setFields["last_used_at"] = now
			update["$inc"] = bson.M{"usage_count": 1}
		}
		if _, err := s.subjects.UpdateByID(ctx, sub.ID, update); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DeleteSubject(ctx context.Context, id bson.ObjectID) error {
	_, err := s.subjects.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// --- Ideas Inbox ---

func (s *Store) ListPendingIdeas(ctx context.Context) ([]domain.Idea, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cur, err := s.ideas.Find(ctx, bson.M{"used": false}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var list []domain.Idea
	if err := cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) CreateIdea(ctx context.Context, content, subjectName string) (domain.Idea, error) {
	idea := domain.Idea{
		ID:          bson.NewObjectID(),
		Content:     content,
		SubjectName: subjectName,
		Used:        false,
		CreatedAt:   time.Now().UTC(),
	}
	_, err := s.ideas.InsertOne(ctx, idea)
	return idea, err
}

func (s *Store) MarkIdeaUsed(ctx context.Context, id bson.ObjectID, postID string) error {
	now := time.Now().UTC()
	_, err := s.ideas.UpdateByID(ctx, id, bson.M{
		"$set": bson.M{
			"used":         true,
			"used_post_id": postID,
			"used_at":      now,
		},
	})
	return err
}

func (s *Store) DeleteIdea(ctx context.Context, id bson.ObjectID) error {
	_, err := s.ideas.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

// --- Posts & GridFS Images ---

func (s *Store) CountWeekPosts(ctx context.Context, weekKey string) (int64, error) {
	return s.posts.CountDocuments(ctx, bson.M{
		"week_key": weekKey,
		"status":   bson.M{"$nin": []domain.PostStatus{domain.StatusArchived, domain.StatusFailed, domain.StatusGenerating}},
	})
}

// CleanupStuckGeneratingPosts removes orphan placeholder posts that got stuck in StatusGenerating
// (e.g. due to server restart, network timeout, or process interruption).
func (s *Store) CleanupStuckGeneratingPosts(ctx context.Context, olderThan time.Duration) (int64, error) {
	threshold := time.Now().UTC().Add(-olderThan)
	res, err := s.posts.DeleteMany(ctx, bson.M{
		"status":     domain.StatusGenerating,
		"created_at": bson.M{"$lt": threshold},
	})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (s *Store) ListPostsByWeek(ctx context.Context, weekKey string) ([]domain.Post, error) {
	opts := options.Find().SetSort(bson.D{{Key: "slot_index", Value: 1}, {Key: "created_at", Value: 1}})
	cur, err := s.posts.Find(ctx, bson.M{"week_key": weekKey}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var list []domain.Post
	if err := cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) ListPostsMissingImages(ctx context.Context, weekKey string) ([]domain.Post, error) {
	filter := bson.M{
		"status":  bson.M{"$in": []domain.PostStatus{domain.StatusDraft, domain.StatusScheduled}},
		"content": bson.M{"$ne": ""},
		"$or": []bson.M{
			{"image_file_id": bson.M{"$exists": false}},
			{"image_file_id": bson.NilObjectID},
		},
	}
	if weekKey != "" {
		filter["week_key"] = weekKey
	}
	opts := options.Find().SetSort(bson.D{{Key: "slot_index", Value: 1}, {Key: "created_at", Value: 1}})
	cur, err := s.posts.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("list posts missing images: %w", err)
	}
	defer cur.Close(ctx)

	var list []domain.Post
	if err := cur.All(ctx, &list); err != nil {
		return nil, fmt.Errorf("decode posts missing images: %w", err)
	}
	return list, nil
}

func (s *Store) ListAvailableWeeks(ctx context.Context, currentWeekKey string) ([]string, error) {
	var weeks []string
	err := s.posts.Distinct(ctx, "week_key", bson.M{}).Decode(&weeks)
	if err != nil {
		return []string{currentWeekKey}, nil
	}
	hasCurrent := false
	for _, w := range weeks {
		if w == currentWeekKey {
			hasCurrent = true
			break
		}
	}
	if !hasCurrent {
		weeks = append(weeks, currentWeekKey)
	}
	// Sort descending (newest week first)
	for i := 0; i < len(weeks); i++ {
		for j := i + 1; j < len(weeks); j++ {
			if weeks[j] > weeks[i] {
				weeks[i], weeks[j] = weeks[j], weeks[i]
			}
		}
	}
	return weeks, nil
}

func (s *Store) ListRecentPostsForSubject(ctx context.Context, subjectName string, limit int64) ([]domain.Post, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetLimit(limit)
	cur, err := s.posts.Find(ctx, bson.M{
		"subject_name": subjectName,
		"status":       bson.M{"$ne": domain.StatusFailed},
	}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var list []domain.Post
	if err := cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) ListGoldenPosts(ctx context.Context, limit int64) ([]domain.Post, error) {
	opts := options.Find().
		SetSort(bson.D{{Key: "updated_at", Value: -1}}).
		SetLimit(limit)
	cur, err := s.posts.Find(ctx, bson.M{"is_golden": true}, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var list []domain.Post
	if err := cur.All(ctx, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) GetPost(ctx context.Context, id bson.ObjectID) (domain.Post, error) {
	var p domain.Post
	err := s.posts.FindOne(ctx, bson.M{"_id": id}).Decode(&p)
	return p, err
}

func (s *Store) InsertPost(ctx context.Context, p domain.Post) (domain.Post, error) {
	if p.ID.IsZero() {
		p.ID = bson.NewObjectID()
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := s.posts.InsertOne(ctx, p)
	return p, err
}

func (s *Store) SavePost(ctx context.Context, p domain.Post) error {
	p.UpdatedAt = time.Now().UTC()
	_, err := s.posts.ReplaceOne(ctx, bson.M{"_id": p.ID}, p)
	return err
}

func (s *Store) DeletePost(ctx context.Context, id bson.ObjectID) error {
	p, err := s.GetPost(ctx, id)
	if err == nil && !p.ImageFileID.IsZero() {
		_ = s.bucket.Delete(ctx, p.ImageFileID)
	}
	_, err = s.posts.DeleteOne(ctx, bson.M{"_id": id})
	return err
}

func (s *Store) UploadImage(ctx context.Context, filename string, data []byte, previousFileID bson.ObjectID) (bson.ObjectID, error) {
	fileID, err := s.bucket.UploadFromStream(ctx, filename, bytes.NewReader(data))
	if err != nil {
		return bson.ObjectID{}, fmt.Errorf("gridfs upload: %w", err)
	}
	if !previousFileID.IsZero() {
		_ = s.bucket.Delete(ctx, previousFileID)
	}
	return fileID, nil
}

func (s *Store) StreamImage(ctx context.Context, fileID bson.ObjectID, w io.Writer) error {
	_, err := s.bucket.DownloadToStream(ctx, fileID, w)
	return err
}
