package gateway

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq" // PostgreSQL 驱动
	tlog "github.com/streasure/treasure-slog"
)

// Database 数据库管理器
type Database struct {
	db             *sql.DB
	ctx            context.Context
	cancel         context.CancelFunc
	config         DatabaseConfig
	isConnected    bool
	reconnectCount int
}

// DatabaseConfig 数据库配置
type DatabaseConfig struct {
	Host         string
	Port         int
	User         string
	Password     string
	Database     string
	MaxIdleConns int
	MaxOpenConns int
	ConnTimeout  time.Duration
	RetryInterval time.Duration
	MaxRetries    int
}

// User 用户模型
type User struct {
	UserUUID    string    `json:"user_uuid"`
	Username    string    `json:"username"`
	Email       string    `json:"email"`
	Phone       string    `json:"phone"`
	Status      string    `json:"status"`
	LastLogin   time.Time `json:"last_login"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MessageRecord 消息记录
type MessageRecord struct {
	ID          string    `json:"id"`
	ConnectionID string    `json:"connection_id"`
	UserUUID    string    `json:"user_uuid"`
	Route       string    `json:"route"`
	Payload     string    `json:"payload"`
	Direction   string    `json:"direction"` // incoming, outgoing
	Status      string    `json:"status"`    // pending, sent, delivered, failed
	Sequence    int64     `json:"sequence"`
	Error       string    `json:"error"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ConnectionRecord 连接记录
type ConnectionRecord struct {
	ID          string    `json:"id"`
	ConnectionID string    `json:"connection_id"`
	UserUUID    string    `json:"user_uuid"`
	RemoteAddr  string    `json:"remote_addr"`
	LocalAddr   string    `json:"local_addr"`
	Protocol    string    `json:"protocol"` // tcp, udp, websocket
	Status      string    `json:"status"`    // connected, disconnected
	LastActive  time.Time `json:"last_active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// GroupRecord 组记录
type GroupRecord struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"group_id"`
	GroupName string    `json:"group_name"`
	UserCount int       `json:"user_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GroupUserRecord 组用户记录
type GroupUserRecord struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"group_id"`
	UserUUID  string    `json:"user_uuid"`
	Role      string    `json:"role"` // admin, member
	CreatedAt time.Time `json:"created_at"`
}

// NewDatabase 创建数据库管理器
func NewDatabase(config DatabaseConfig) *Database {
	ctx, cancel := context.WithCancel(context.Background())

	db := &Database{
		ctx:     ctx,
		cancel:  cancel,
		config:  config,
	}

	// 初始化数据库连接
	go db.connect()

	return db
}

// connect 连接数据库
func (d *Database) connect() {
	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		d.config.Host, d.config.Port, d.config.User, d.config.Password, d.config.Database,
	)

	var err error
	for i := 0; i < d.config.MaxRetries; i++ {
		d.db, err = sql.Open("postgres", connStr)
		if err == nil {
			d.db.SetMaxIdleConns(d.config.MaxIdleConns)
			d.db.SetMaxOpenConns(d.config.MaxOpenConns)
			d.db.SetConnMaxLifetime(time.Hour)

			// 测试连接
			err = d.db.Ping()
			if err == nil {
				d.isConnected = true
				d.reconnectCount = 0
				tlog.Info("数据库连接成功", "host", d.config.Host, "port", d.config.Port, "database", d.config.Database)

				// 初始化表结构
				d.initializeSchema()
				return
			}
		}

		tlog.Error("数据库连接失败", "error", err, "retry", i+1, "maxRetries", d.config.MaxRetries)
		time.Sleep(d.config.RetryInterval)
	}

	tlog.Error("数据库连接失败，达到最大重试次数", "error", err)
}

// initializeSchema 初始化表结构
func (d *Database) initializeSchema() {
	queries := []string{
		// 用户表
		`CREATE TABLE IF NOT EXISTS users (
			user_uuid VARCHAR(36) PRIMARY KEY,
			username VARCHAR(255) NOT NULL,
			email VARCHAR(255) UNIQUE,
			phone VARCHAR(20) UNIQUE,
			status VARCHAR(20) DEFAULT 'active',
			last_login TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,

		// 消息表
		`CREATE TABLE IF NOT EXISTS messages (
			id SERIAL PRIMARY KEY,
			connection_id VARCHAR(36) NOT NULL,
			user_uuid VARCHAR(36),
			route VARCHAR(100) NOT NULL,
			payload TEXT,
			direction VARCHAR(10) NOT NULL, -- incoming, outgoing
			status VARCHAR(20) DEFAULT 'pending', -- pending, sent, delivered, failed
			sequence BIGINT,
			error TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,

		// 连接表
		`CREATE TABLE IF NOT EXISTS connections (
			id SERIAL PRIMARY KEY,
			connection_id VARCHAR(36) PRIMARY KEY,
			user_uuid VARCHAR(36),
			remote_addr VARCHAR(50) NOT NULL,
			local_addr VARCHAR(50) NOT NULL,
			protocol VARCHAR(20) NOT NULL, -- tcp, udp, websocket
			status VARCHAR(20) DEFAULT 'connected', -- connected, disconnected
			last_active TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,

		// 组表
		`CREATE TABLE IF NOT EXISTS groups (
			id SERIAL PRIMARY KEY,
			group_id VARCHAR(36) PRIMARY KEY,
			group_name VARCHAR(255) NOT NULL,
			user_count INT DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,

		// 组用户表
		`CREATE TABLE IF NOT EXISTS group_users (
			id SERIAL PRIMARY KEY,
			group_id VARCHAR(36) NOT NULL,
			user_uuid VARCHAR(36) NOT NULL,
			role VARCHAR(20) DEFAULT 'member', -- admin, member
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(group_id, user_uuid)
		)`,

		// 索引
		`CREATE INDEX IF NOT EXISTS idx_messages_user_uuid ON messages(user_uuid)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_connection_id ON messages(connection_id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_sequence ON messages(sequence)`,
		`CREATE INDEX IF NOT EXISTS idx_connections_user_uuid ON connections(user_uuid)`,
		`CREATE INDEX IF NOT EXISTS idx_connections_connection_id ON connections(connection_id)`,
		`CREATE INDEX IF NOT EXISTS idx_group_users_group_id ON group_users(group_id)`,
		`CREATE INDEX IF NOT EXISTS idx_group_users_user_uuid ON group_users(user_uuid)`,
	}

	for _, query := range queries {
		_, err := d.db.ExecContext(d.ctx, query)
		if err != nil {
			tlog.Error("初始化表结构失败", "error", err, "query", query)
		}
	}

	tlog.Info("数据库表结构初始化完成")
}

// Close 关闭数据库连接
func (d *Database) Close() error {
	d.cancel()
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

// IsConnected 检查数据库连接状态
func (d *Database) IsConnected() bool {
	if d.db == nil {
		return false
	}

	err := d.db.Ping()
	if err != nil {
		d.isConnected = false
		tlog.Error("数据库连接已断开", "error", err)
		// 尝试重连
		go d.connect()
		return false
	}

	d.isConnected = true
	return true
}

// SaveUser 保存用户
func (d *Database) SaveUser(user *User) error {
	if !d.IsConnected() {
		return fmt.Errorf("database not connected")
	}

	query := `
		INSERT INTO users (user_uuid, username, email, phone, status, last_login, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_uuid) DO UPDATE SET
			username = $2,
			email = $3,
			phone = $4,
			status = $5,
			last_login = $6,
			updated_at = $8
	`

	_, err := d.db.ExecContext(d.ctx, query,
		user.UserUUID,
		user.Username,
		user.Email,
		user.Phone,
		user.Status,
		user.LastLogin,
		user.CreatedAt,
		user.UpdatedAt,
	)

	if err != nil {
		tlog.Error("保存用户失败", "error", err, "userUUID", user.UserUUID)
		return err
	}

	return nil
}

// GetUser 获取用户
func (d *Database) GetUser(userUUID string) (*User, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("database not connected")
	}

	query := `
		SELECT user_uuid, username, email, phone, status, last_login, created_at, updated_at
		FROM users
		WHERE user_uuid = $1
	`

	row := d.db.QueryRowContext(d.ctx, query, userUUID)

	var user User
	err := row.Scan(
		&user.UserUUID,
		&user.Username,
		&user.Email,
		&user.Phone,
		&user.Status,
		&user.LastLogin,
		&user.CreatedAt,
		&user.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		tlog.Error("获取用户失败", "error", err, "userUUID", userUUID)
		return nil, err
	}

	return &user, nil
}

// SaveMessage 保存消息
func (d *Database) SaveMessage(msg *MessageRecord) error {
	if !d.IsConnected() {
		return fmt.Errorf("database not connected")
	}

	query := `
		INSERT INTO messages (connection_id, user_uuid, route, payload, direction, status, sequence, error, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`

	var id int
	err := d.db.QueryRowContext(d.ctx, query,
		msg.ConnectionID,
		msg.UserUUID,
		msg.Route,
		msg.Payload,
		msg.Direction,
		msg.Status,
		msg.Sequence,
		msg.Error,
		msg.CreatedAt,
		msg.UpdatedAt,
	).Scan(&id)

	if err != nil {
		tlog.Error("保存消息失败", "error", err, "connectionID", msg.ConnectionID)
		return err
	}

	msg.ID = fmt.Sprintf("%d", id)
	return nil
}

// GetMessages 获取用户消息
func (d *Database) GetMessages(userUUID string, limit int) ([]*MessageRecord, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("database not connected")
	}

	query := `
		SELECT id, connection_id, user_uuid, route, payload, direction, status, sequence, error, created_at, updated_at
		FROM messages
		WHERE user_uuid = $1
		ORDER BY created_at DESC
		LIMIT $2
	`

	rows, err := d.db.QueryContext(d.ctx, query, userUUID, limit)
	if err != nil {
		tlog.Error("获取消息失败", "error", err, "userUUID", userUUID)
		return nil, err
	}
	defer rows.Close()

	var messages []*MessageRecord
	for rows.Next() {
		var msg MessageRecord
		err := rows.Scan(
			&msg.ID,
			&msg.ConnectionID,
			&msg.UserUUID,
			&msg.Route,
			&msg.Payload,
			&msg.Direction,
			&msg.Status,
			&msg.Sequence,
			&msg.Error,
			&msg.CreatedAt,
			&msg.UpdatedAt,
		)
		if err != nil {
			tlog.Error("扫描消息失败", "error", err)
			continue
		}
		messages = append(messages, &msg)
	}

	return messages, nil
}

// SaveConnection 保存连接
func (d *Database) SaveConnection(conn *ConnectionRecord) error {
	if !d.IsConnected() {
		return fmt.Errorf("database not connected")
	}

	query := `
		INSERT INTO connections (connection_id, user_uuid, remote_addr, local_addr, protocol, status, last_active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (connection_id) DO UPDATE SET
			user_uuid = $2,
			remote_addr = $3,
			local_addr = $4,
			protocol = $5,
			status = $6,
			last_active = $7,
			updated_at = $9
	`

	_, err := d.db.ExecContext(d.ctx, query,
		conn.ConnectionID,
		conn.UserUUID,
		conn.RemoteAddr,
		conn.LocalAddr,
		conn.Protocol,
		conn.Status,
		conn.LastActive,
		conn.CreatedAt,
		conn.UpdatedAt,
	)

	if err != nil {
		tlog.Error("保存连接失败", "error", err, "connectionID", conn.ConnectionID)
		return err
	}

	return nil
}

// GetConnection 获取连接
func (d *Database) GetConnection(connectionID string) (*ConnectionRecord, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("database not connected")
	}

	query := `
		SELECT id, connection_id, user_uuid, remote_addr, local_addr, protocol, status, last_active, created_at, updated_at
		FROM connections
		WHERE connection_id = $1
	`

	row := d.db.QueryRowContext(d.ctx, query, connectionID)

	var conn ConnectionRecord
	err := row.Scan(
		&conn.ID,
		&conn.ConnectionID,
		&conn.UserUUID,
		&conn.RemoteAddr,
		&conn.LocalAddr,
		&conn.Protocol,
		&conn.Status,
		&conn.LastActive,
		&conn.CreatedAt,
		&conn.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		tlog.Error("获取连接失败", "error", err, "connectionID", connectionID)
		return nil, err
	}

	return &conn, nil
}

// SaveGroup 保存组
func (d *Database) SaveGroup(group *GroupRecord) error {
	if !d.IsConnected() {
		return fmt.Errorf("database not connected")
	}

	query := `
		INSERT INTO groups (group_id, group_name, user_count, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (group_id) DO UPDATE SET
			group_name = $2,
			user_count = $3,
			updated_at = $5
	`

	_, err := d.db.ExecContext(d.ctx, query,
		group.GroupID,
		group.GroupName,
		group.UserCount,
		group.CreatedAt,
		group.UpdatedAt,
	)

	if err != nil {
		tlog.Error("保存组失败", "error", err, "groupID", group.GroupID)
		return err
	}

	return nil
}

// AddUserToGroup 添加用户到组
func (d *Database) AddUserToGroup(groupID, userUUID, role string) error {
	if !d.IsConnected() {
		return fmt.Errorf("database not connected")
	}

	// 开始事务
	tx, err := d.db.BeginTx(d.ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// 添加用户到组
	query := `
		INSERT INTO group_users (group_id, user_uuid, role, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (group_id, user_uuid) DO NOTHING
	`
	_, err = tx.ExecContext(d.ctx, query, groupID, userUUID, role, time.Now())
	if err != nil {
		return err
	}

	// 更新组用户数
	query = `
		UPDATE groups
		SET user_count = (SELECT COUNT(*) FROM group_users WHERE group_id = $1),
			updated_at = $2
		WHERE group_id = $1
	`
	_, err = tx.ExecContext(d.ctx, query, groupID, time.Now())
	if err != nil {
		return err
	}

	// 提交事务
	return tx.Commit()
}

// GetGroupUsers 获取组用户
func (d *Database) GetGroupUsers(groupID string) ([]string, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("database not connected")
	}

	query := `
		SELECT user_uuid
		FROM group_users
		WHERE group_id = $1
	`

	rows, err := d.db.QueryContext(d.ctx, query, groupID)
	if err != nil {
		tlog.Error("获取组用户失败", "error", err, "groupID", groupID)
		return nil, err
	}
	defer rows.Close()

	var userUUIDs []string
	for rows.Next() {
		var userUUID string
		err := rows.Scan(&userUUID)
		if err != nil {
			tlog.Error("扫描用户UUID失败", "error", err)
			continue
		}
		userUUIDs = append(userUUIDs, userUUID)
	}

	return userUUIDs, nil
}

// GetUserGroups 获取用户的组
func (d *Database) GetUserGroups(userUUID string) ([]string, error) {
	if !d.IsConnected() {
		return nil, fmt.Errorf("database not connected")
	}

	query := `
		SELECT group_id
		FROM group_users
		WHERE user_uuid = $1
	`

	rows, err := d.db.QueryContext(d.ctx, query, userUUID)
	if err != nil {
		tlog.Error("获取用户组失败", "error", err, "userUUID", userUUID)
		return nil, err
	}
	defer rows.Close()

	var groupIDs []string
	for rows.Next() {
		var groupID string
		err := rows.Scan(&groupID)
		if err != nil {
			tlog.Error("扫描组ID失败", "error", err)
			continue
		}
		groupIDs = append(groupIDs, groupID)
	}

	return groupIDs, nil
}
