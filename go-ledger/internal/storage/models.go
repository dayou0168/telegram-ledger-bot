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
	ActiveDayKey        string
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
	UserID                              int64
	Status                              string
	Remark                              string
	CreatedBy                           int64
	PrivateCleanupEnabled               bool
	PrivateCleanupTime                  string
	PrivateCleanupLastRunDate           string
	PrivateCleanupBotDeleteAfterSeconds int
	PrivateCleanupIncomingEnabled       bool
	PrivateCleanupIncomingAfterSeconds  int
	PrivateCleanupScope                 string
	CreatedAt                           time.Time
	UpdatedAt                           time.Time
}

type GlobalOperator struct {
	UserID                              int64
	Level                               string
	Status                              string
	ParentUserID                        int64
	Remark                              string
	CreatedBy                           int64
	CreatedAt                           time.Time
	DisabledBy                          int64
	DisabledAt                          *time.Time
	PrivateCleanupEnabled               bool
	PrivateCleanupTime                  string
	PrivateCleanupLastRunDate           string
	PrivateCleanupBotDeleteAfterSeconds int
	PrivateCleanupIncomingEnabled       bool
	PrivateCleanupIncomingAfterSeconds  int
	PrivateCleanupScope                 string
	UpdatedAt                           time.Time
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

type PrivateChatMessage struct {
	ID                  int64
	OperatorUserID      int64
	ChatID              int64
	MessageID           int64
	Direction           string
	Category            string
	CleanupAfterSeconds int
	DueAt               *time.Time
	CreatedAt           time.Time
	DeletedAt           *time.Time
	LastError           string
}

type PrivateCleanupTarget struct {
	UserID      int64
	CleanupTime string
}

type PrivateCleanupSettings struct {
	Enabled             bool
	DailyTime           string
	DailyLastRunDate    string
	BotDeleteAfter      int
	IncomingEnabled     bool
	IncomingDeleteAfter int
	Scope               string
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

type RecordDaySummary struct {
	DepositCount     int64
	PayoutCount      int64
	TotalDepositCNY  string
	TotalDepositUSDT string
	TotalPayoutUSDT  string
}

type BillSummaryData struct {
	Records []Record
	Summary RecordDaySummary
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

type ChainWatcherBot struct {
	BotID     string
	Secret    string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ChainWatcherSubscription struct {
	BotID           string
	ChatID          int64
	OwnerUserID     int64
	Address         string
	Label           string
	WatchIncome     bool
	WatchExpense    bool
	NotifyTRX       bool
	MinNotifyAmount string
	Active          bool
	UpdatedAt       time.Time
}

type ChainWatcherEvent struct {
	EventID        string
	TxHash         string
	Contract       string
	From           string
	To             string
	Value          string
	TokenSymbol    string
	TokenAddress   string
	TokenDecimals  int
	BlockTimestamp int64
	Confirmed      bool
	Source         string
	EventIndex     string
}

type ChainWatcherMatchedEvent struct {
	DeliveryID     string
	EventID        string
	BotID          string
	ChatID         int64
	OwnerUserID    int64
	WatchAddress   string
	Label          string
	Direction      string
	TxHash         string
	From           string
	To             string
	Value          string
	TokenSymbol    string
	TokenAddress   string
	TokenDecimals  int
	BlockTimestamp int64
	Confirmed      bool
	Status         string
	Attempts       int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeliveredAt    *time.Time
}

type ChainWatcherDeliveryStats struct {
	PendingCount       int64
	DeliveringCount    int64
	OldestPendingAt    *time.Time
	OldestPendingAgeMS int64
}

type ChainWatcherCleanupStats struct {
	MatchedDeleted int64
	EventsDeleted  int64
}

type ChainWatcherWatermark struct {
	Timestamp int64
	TxHash    string
	Source    string
	UpdatedAt time.Time
}

type ChainWatcherCatchupState struct {
	Required  bool
	Reason    string
	UpdatedAt time.Time
}

type ChainWatcherFallbackLease struct {
	LeaseName          string
	HolderID           string
	LeaseUntil         time.Time
	Mode               string
	StartedAt          *time.Time
	LastWatcherSuccess *time.Time
	FallbackRequests   int64
	Fallback429        int64
	CatchupFrom        int64
	CatchupTo          int64
	CatchupPages       int64
	CatchupBudgetUsed  int64
	UpdatedAt          time.Time
}

type WatchSettings struct {
	OwnerUserID     int64
	WatchIncome     bool
	WatchExpense    bool
	NotifyTRX       bool
	MinNotifyAmount string
	UpdatedAt       time.Time
}

type AdminLoginTicket struct {
	TokenHash string
	UserID    int64
	Role      string
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
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

type NotificationOutboxStats struct {
	Pending        int64                           `json:"pending"`
	Sending        int64                           `json:"sending"`
	Sent           int64                           `json:"sent"`
	Failed         int64                           `json:"failed"`
	OldestPending  *time.Time                      `json:"oldest_pending,omitempty"`
	LastError      string                          `json:"last_error,omitempty"`
	ByPriority     []NotificationPriorityCount     `json:"by_priority"`
	FailureClasses []NotificationFailureClassCount `json:"failure_classes"`
}

type NotificationOutboxCleanupStats struct {
	SentDeleted   int64 `json:"sent_deleted"`
	FailedDeleted int64 `json:"failed_deleted"`
}

type NotificationPriorityCount struct {
	Priority int    `json:"priority"`
	Status   string `json:"status"`
	Count    int64  `json:"count"`
}

type NotificationFailureClassCount struct {
	Class string `json:"class"`
	Count int64  `json:"count"`
}
