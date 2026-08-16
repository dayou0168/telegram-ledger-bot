package chainwatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	pendingSchemaV1    = "tron.pending.v1"
	confirmedSchemaV1  = "tron.confirmed.v1"
	tronBase58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
)

type confirmedEnvelope struct {
	Schema           string          `json:"schema"`
	ObservedAt       int64           `json:"observedAt"`
	TxID             string          `json:"txId"`
	Kind             string          `json:"kind"`
	Timestamp        int64           `json:"timestamp"`
	BlockNumber      int64           `json:"blockNumber"`
	TransactionIndex json.RawMessage `json:"transactionIndex"`
	Status           string          `json:"status"`
	FromAddress      string          `json:"fromAddress"`
	ToAddress        string          `json:"toAddress"`
	Amount           json.RawMessage `json:"amount"`
	TokenSymbol      string          `json:"tokenSymbol"`
	TokenAddress     string          `json:"tokenAddress"`
	TokenDecimals    int             `json:"tokenDecimals"`
}

func ParseConfirmedTransfer(raw []byte, usdtContract string) (tron.Transfer, bool, error) {
	var envelope confirmedEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return tron.Transfer{}, false, fmt.Errorf("decode confirmed envelope: %w", err)
	}
	if envelope.Schema != confirmedSchemaV1 {
		return tron.Transfer{}, false, nil
	}
	txID := strings.ToLower(strings.TrimSpace(envelope.TxID))
	if txID == "" {
		return tron.Transfer{}, false, errors.New("confirmed transaction id is empty")
	}
	amount, err := rawDecimal(envelope.Amount)
	if err != nil {
		return tron.Transfer{}, false, fmt.Errorf("decode confirmed amount: %w", err)
	}
	symbol := strings.ToUpper(strings.TrimSpace(envelope.TokenSymbol))
	tokenAddress := strings.TrimSpace(envelope.TokenAddress)
	if symbol == "TRX" {
		tokenAddress = "trx"
	} else if symbol == "USDT" {
		if !strings.EqualFold(tokenAddress, usdtContract) {
			return tron.Transfer{}, false, nil
		}
		tokenAddress = strings.TrimSpace(usdtContract)
	} else {
		return tron.Transfer{}, false, nil
	}
	from, to := strings.TrimSpace(envelope.FromAddress), strings.TrimSpace(envelope.ToAddress)
	if from == "" || to == "" {
		return tron.Transfer{}, false, errors.New("confirmed transaction addresses are incomplete")
	}
	index := strings.Trim(string(envelope.TransactionIndex), `"`)
	status := strings.ToUpper(strings.TrimSpace(envelope.Status))
	if status != "SUCCESS" {
		status = "FAILED"
	}
	return tron.Transfer{
		Hash: txID, From: from, To: to, Value: amount,
		TokenSymbol: symbol, TokenAddress: tokenAddress, TokenDecimals: envelope.TokenDecimals,
		BlockTimestamp: envelope.Timestamp, Confirmed: true, Result: status, EventIndex: index,
	}, true, nil
}

type pendingEnvelope struct {
	Schema        string          `json:"schema"`
	Source        string          `json:"source"`
	ObservedAt    int64           `json:"observedAt"`
	TxID          string          `json:"txId"`
	Kind          string          `json:"kind"`
	ContractType  string          `json:"contractType"`
	Timestamp     int64           `json:"timestamp"`
	Expiration    int64           `json:"expiration"`
	ContractValue pendingContract `json:"contractValue"`
}

type pendingContract struct {
	OwnerAddress    string          `json:"owner_address"`
	ToAddress       string          `json:"to_address"`
	ContractAddress string          `json:"contract_address"`
	Data            string          `json:"data"`
	Amount          json.RawMessage `json:"amount"`
}

func ParsePendingTransfer(raw []byte, usdtContract string) (tron.Transfer, bool, error) {
	var envelope pendingEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return tron.Transfer{}, false, fmt.Errorf("decode pending envelope: %w", err)
	}
	if envelope.Schema != pendingSchemaV1 {
		return tron.Transfer{}, false, nil
	}
	txID := strings.ToLower(strings.TrimSpace(envelope.TxID))
	if txID == "" {
		return tron.Transfer{}, false, errors.New("pending transaction id is empty")
	}
	timestamp := envelope.Timestamp
	if timestamp <= 0 {
		timestamp = envelope.ObservedAt
	}

	switch envelope.Kind {
	case "trx_transfer":
		if envelope.ContractType != "TransferContract" {
			return tron.Transfer{}, false, nil
		}
		amount, err := rawDecimal(envelope.ContractValue.Amount)
		if err != nil {
			return tron.Transfer{}, false, fmt.Errorf("decode TRX amount: %w", err)
		}
		from := strings.TrimSpace(envelope.ContractValue.OwnerAddress)
		to := strings.TrimSpace(envelope.ContractValue.ToAddress)
		if from == "" || to == "" {
			return tron.Transfer{}, false, errors.New("pending TRX addresses are incomplete")
		}
		return tron.Transfer{
			Hash: txID, From: from, To: to, Value: amount,
			TokenSymbol: "TRX", TokenAddress: "trx", TokenDecimals: 6,
			BlockTimestamp: timestamp, Confirmed: false,
		}, true, nil

	case "usdt_transfer", "usdt_transfer_from":
		if envelope.ContractType != "TriggerSmartContract" ||
			!strings.EqualFold(strings.TrimSpace(envelope.ContractValue.ContractAddress), strings.TrimSpace(usdtContract)) {
			return tron.Transfer{}, false, nil
		}
		from, to, amount, err := decodeUSDTCall(envelope.Kind, envelope.ContractValue.OwnerAddress, envelope.ContractValue.Data)
		if err != nil {
			return tron.Transfer{}, false, err
		}
		return tron.Transfer{
			Hash: txID, From: from, To: to, Value: amount,
			TokenSymbol: "USDT", TokenAddress: strings.TrimSpace(usdtContract), TokenDecimals: 6,
			BlockTimestamp: timestamp, Confirmed: false,
		}, true, nil
	default:
		return tron.Transfer{}, false, nil
	}
}

func rawDecimal(raw json.RawMessage) (string, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return "", errors.New("amount is empty")
	}
	if strings.HasPrefix(value, `"`) {
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
	}
	integer, ok := new(big.Int).SetString(strings.TrimSpace(value), 10)
	if !ok || integer.Sign() < 0 {
		return "", fmt.Errorf("invalid unsigned decimal %q", value)
	}
	return integer.String(), nil
}

func decodeUSDTCall(kind, owner, calldata string) (string, string, string, error) {
	data := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(calldata)), "0x")
	selector := "a9059cbb"
	words := 2
	if kind == "usdt_transfer_from" {
		selector = "23b872dd"
		words = 3
	}
	if !strings.HasPrefix(data, selector) || len(data) < 8+64*words {
		return "", "", "", fmt.Errorf("invalid %s calldata", kind)
	}
	data = data[8:]
	word := func(index int) string { return data[index*64 : (index+1)*64] }
	decodeAddress := func(value string) (string, error) {
		return tronBase58FromHex20(value[len(value)-40:])
	}

	var from, to string
	var err error
	amountWord := ""
	if kind == "usdt_transfer_from" {
		from, err = decodeAddress(word(0))
		if err != nil {
			return "", "", "", err
		}
		to, err = decodeAddress(word(1))
		amountWord = word(2)
	} else {
		from = strings.TrimSpace(owner)
		to, err = decodeAddress(word(0))
		amountWord = word(1)
	}
	if err != nil {
		return "", "", "", err
	}
	if from == "" {
		return "", "", "", errors.New("USDT owner address is empty")
	}
	amount, ok := new(big.Int).SetString(amountWord, 16)
	if !ok {
		return "", "", "", errors.New("invalid USDT amount")
	}
	return from, to, amount.String(), nil
}

func tronBase58FromHex20(value string) (string, error) {
	address, err := hex.DecodeString(value)
	if err != nil || len(address) != 20 {
		return "", errors.New("invalid TRON address word")
	}
	payload := append([]byte{0x41}, address...)
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	checked := append(payload, second[:4]...)
	return base58Encode(checked), nil
}

func base58Encode(value []byte) string {
	integer := new(big.Int).SetBytes(value)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)
	encoded := make([]byte, 0, 35)
	for integer.Cmp(zero) > 0 {
		integer.DivMod(integer, base, mod)
		encoded = append(encoded, tronBase58Alphabet[mod.Int64()])
	}
	for _, b := range value {
		if b != 0 {
			break
		}
		encoded = append(encoded, tronBase58Alphabet[0])
	}
	for left, right := 0, len(encoded)-1; left < right; left, right = left+1, right-1 {
		encoded[left], encoded[right] = encoded[right], encoded[left]
	}
	return string(encoded)
}

func (s *Server) kafkaPendingEnabled() bool {
	return s.cfg.SourceMode == "kafka" && len(s.cfg.KafkaBrokers) > 0 &&
		strings.TrimSpace(s.cfg.KafkaPendingTopic) != "" && strings.TrimSpace(s.cfg.KafkaConfirmedTopic) != ""
}

func (s *Server) setKafkaHealth(connected bool, confirmedAt time.Time, err error) {
	s.kafkaMu.Lock()
	s.kafkaConnected = connected
	if connected {
		s.kafkaLastRecordAt = time.Now()
	}
	if !confirmedAt.IsZero() {
		s.kafkaLastConfirmed = confirmedAt
	}
	if err != nil {
		s.kafkaLastError = err.Error()
	} else if connected {
		s.kafkaLastError = ""
	}
	s.kafkaMu.Unlock()
}

func (s *Server) kafkaHealthy(now time.Time) (bool, string) {
	s.kafkaMu.RLock()
	connected, lastRecord, lastConfirmed, lastError := s.kafkaConnected, s.kafkaLastRecordAt, s.kafkaLastConfirmed, s.kafkaLastError
	s.kafkaMu.RUnlock()
	staleAfter := s.cfg.KafkaStaleAfter
	if staleAfter <= 0 {
		staleAfter = 15 * time.Second
	}
	reference := lastConfirmed
	if reference.IsZero() {
		reference = lastRecord
	}
	if !reference.IsZero() && now.Sub(reference) <= staleAfter {
		return true, ""
	}
	if reference.IsZero() && !s.startedAt.IsZero() && now.Sub(s.startedAt) <= staleAfter {
		return true, ""
	}
	if !connected {
		return false, lastError
	}
	if reference.IsZero() || now.Sub(reference) > staleAfter {
		return false, "confirmed stream is stale"
	}
	return true, ""
}

func (s *Server) kafkaPendingLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := s.consumeKafkaPending(ctx)
		if ctx.Err() != nil {
			return
		}
		s.setKafkaHealth(false, time.Time{}, err)
		log.Printf("chain watcher kafka stream stopped: %v; retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *Server) consumeKafkaPending(ctx context.Context) error {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(s.cfg.KafkaBrokers...),
		kgo.ClientID("ledger-chain-watcher"),
		kgo.ConsumerGroup(s.cfg.KafkaGroupID),
		kgo.ConsumeTopics(s.cfg.KafkaPendingTopic, s.cfg.KafkaConfirmedTopic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		kgo.DisableAutoCommit(),
		kgo.FetchMaxWait(250*time.Millisecond),
	)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("ping kafka: %w", err)
	}
	s.setKafkaHealth(true, time.Time{}, nil)
	log.Printf("chain watcher kafka connected: brokers=%s topics=%s,%s group=%s",
		strings.Join(s.cfg.KafkaBrokers, ","), s.cfg.KafkaPendingTopic, s.cfg.KafkaConfirmedTopic, s.cfg.KafkaGroupID)

	for ctx.Err() == nil {
		fetches := client.PollFetches(ctx)
		if err := fetches.Err(); err != nil {
			return err
		}
		records := make([]*kgo.Record, 0, fetches.NumRecords())
		for iter := fetches.RecordIter(); !iter.Done(); {
			record := iter.Next()
			var transfer tron.Transfer
			var relevant bool
			var parseErr error
			source := "kafka_pending"
			if record.Topic == s.cfg.KafkaConfirmedTopic {
				transfer, relevant, parseErr = ParseConfirmedTransfer(record.Value, s.cfg.USDTContract)
				source = "kafka_confirmed"
			} else {
				transfer, relevant, parseErr = ParsePendingTransfer(record.Value, s.cfg.USDTContract)
			}
			if parseErr != nil {
				log.Printf("drop invalid kafka transfer record topic=%s partition=%d offset=%d: %v",
					record.Topic, record.Partition, record.Offset, parseErr)
				records = append(records, record)
				continue
			}
			if relevant {
				dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_, byAddress, loadErr := s.loadSubscriptions(dbCtx)
				if loadErr == nil {
					_, _, loadErr = s.recordTransferMatchesSourcePriority(dbCtx, transfer, byAddress, source, true)
				}
				cancel()
				if loadErr != nil {
					return fmt.Errorf("persist kafka transfer tx %s: %w", transfer.Hash, loadErr)
				}
			}
			confirmedAt := time.Time{}
			if record.Topic == s.cfg.KafkaConfirmedTopic {
				confirmedAt = time.UnixMilli(transfer.BlockTimestamp)
				if confirmedAt.IsZero() || transfer.BlockTimestamp <= 0 {
					confirmedAt = time.Now()
				}
			}
			s.setKafkaHealth(true, confirmedAt, nil)
			records = append(records, record)
		}
		if len(records) > 0 {
			commitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := client.CommitRecords(commitCtx, records...)
			cancel()
			if err != nil {
				return fmt.Errorf("commit kafka offsets: %w", err)
			}
		}
	}
	return ctx.Err()
}
