package storage

import "time"

type User struct {
	ID          int64
	Username    string
	DisplayName string
}

type Group struct {
	ChatID              int64
	Title               string
	Active              bool
	BusinessOpen        bool
	OwnerUserID         int64
	DepositRate         string
	PayoutRate          string
	DepositExchangeRate string
	PayoutExchangeRate  string
	ExchangeRateSource  string
	ExchangeRateRank    int
	ExchangeRateOffset  string
	FeeRate             string
	CutoffHour          int
	AllMembersCanRecord bool
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type BroadcastGroup struct {
	Name      string
	ChatIDs   []int64
	ChatNames []string
	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type BroadcastOperator struct {
	UserID    int64
	Status    string
	Remark    string
	CreatedBy int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type BroadcastPermission struct {
	UserID    int64
	Target    string
	ChatID    int64
	GroupName string
	GrantedBy int64
	CreatedAt time.Time
}

type BroadcastDelivery struct {
	ID              int64
	OperatorUserID  int64
	SourceChatID    int64
	SourceMessageID int64
	TargetChatID    int64
	TargetTitle     string
	TargetMessageID int64
	Mode            string
	TargetName      string
	CreatedAt       time.Time
	ReplacedAt      *time.Time
}

type BroadcastReplaceSetting struct {
	Enabled   bool
	Text      string
	ImageName string
	ImageData []byte
	UpdatedAt time.Time
}

type Record struct {
	ID              int64
	ChatID          int64
	DayKey          string
	Kind            string
	Currency        string
	Amount          string
	Rate            string
	FeeRate         string
	ResultUSDT      string
	SubjectUserID   int64
	SubjectName     string
	ActorUserID     int64
	ActorName       string
	SourceMessageID int64
	BotMessageID    int64
	Remark          string
	CreatedAt       time.Time
	DeletedAt       *time.Time
}

type Operator struct {
	ChatID      int64
	UserID      int64
	Role        string
	AddedBy     int64
	CreatedAt   time.Time
	Username    string
	DisplayName string
}

type WatchTarget struct {
	OwnerUserID     int64
	Address         string
	Label           string
	WatchIncome     bool
	WatchExpense    bool
	NotifyTRX       bool
	MinNotifyAmount string
	LatestTimestamp int64
}

type WatchSettings struct {
	OwnerUserID     int64
	WatchIncome     bool
	WatchExpense    bool
	NotifyTRX       bool
	MinNotifyAmount string
	UpdatedAt       time.Time
}

type AddressValidation struct {
	ChatID           int64
	Address          string
	VerifyCount      int
	FirstUserID      int64
	FirstUserName    string
	PreviousUserID   int64
	PreviousUserName string
	LastUserID       int64
	LastUserName     string
	LastSeenAt       time.Time
	CreatedAt        time.Time
}

type NotificationOutbox struct {
	ID               int64
	Kind             string
	DedupeKey        string
	ChatID           int64
	Text             string
	ParseMode        string
	DisablePreview   bool
	ReplyToMessageID int64
	ReplyMarkupJSON  string
	ReferenceKind    string
	ReferenceID      int64
	Priority         int
	Status           string
	Attempts         int
	NextAttemptAt    time.Time
	LastError        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	SentAt           *time.Time
}
