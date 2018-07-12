package transaction

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/MinterTeam/minter-go-node/core/check"
	"github.com/MinterTeam/minter-go-node/core/code"
	"github.com/MinterTeam/minter-go-node/core/state"
	"github.com/MinterTeam/minter-go-node/core/types"
	"github.com/MinterTeam/minter-go-node/crypto"
	"github.com/MinterTeam/minter-go-node/crypto/sha3"
	"github.com/MinterTeam/minter-go-node/formula"
	"github.com/MinterTeam/minter-go-node/log"
	"github.com/MinterTeam/minter-go-node/rlp"
	"github.com/tendermint/tendermint/libs/common"
	"math/big"
	"regexp"
)

var (
	CommissionMultiplier = big.NewInt(10e8)
)

const (
	maxTxLength          = 1024
	maxPayloadLength     = 128
	maxServiceDataLength = 128

	minCommission = 0
	maxCommission = 100
	unboundPeriod = 518400

	allowedCoinSymbols = "^[A-Z0-9]{3,10}$"
)

type Response struct {
	Code      uint32          `protobuf:"varint,1,opt,name=code,proto3" json:"code,omitempty"`
	Data      []byte          `protobuf:"bytes,2,opt,name=data,proto3" json:"data,omitempty"`
	Log       string          `protobuf:"bytes,3,opt,name=log,proto3" json:"log,omitempty"`
	Info      string          `protobuf:"bytes,4,opt,name=info,proto3" json:"info,omitempty"`
	GasWanted int64           `protobuf:"varint,5,opt,name=gas_wanted,json=gasWanted,proto3" json:"gas_wanted,omitempty"`
	GasUsed   int64           `protobuf:"varint,6,opt,name=gas_used,json=gasUsed,proto3" json:"gas_used,omitempty"`
	Tags      []common.KVPair `protobuf:"bytes,7,rep,name=tags" json:"tags,omitempty"`
	Fee       common.KI64Pair `protobuf:"bytes,8,opt,name=fee" json:"fee"`
}

func RunTx(context *state.StateDB, isCheck bool, rawTx []byte, rewardPull *big.Int, currentBlock uint64) Response {

	if len(rawTx) > maxTxLength {
		return Response{
			Code: code.TxTooLarge,
			Log:  "TX length is over 1024 bytes"}
	}

	tx, err := DecodeFromBytes(rawTx)

	if !isCheck {
		log.Info("Deliver tx", "tx", tx.String())
	}

	if err != nil {
		return Response{
			Code: code.DecodeError,
			Log:  err.Error()}
	}

	if len(tx.Payload) > maxPayloadLength {
		return Response{
			Code: code.TxPayloadTooLarge,
			Log:  "TX payload length is over 128 bytes"}
	}

	if len(tx.ServiceData) > maxServiceDataLength {
		return Response{
			Code: code.TxServiceDataTooLarge,
			Log:  "TX service data length is over 128 bytes"}
	}

	sender, err := tx.Sender()

	if err != nil {
		return Response{
			Code: code.DecodeError,
			Log:  err.Error()}
	}

	// do not look at nonce of transaction while checking tx
	// this will allow us to send multiple transaction from one account in one block
	// in the future we should use "last known nonce" approach from Ethereum
	if !isCheck {
		if expectedNonce := context.GetNonce(sender) + 1; expectedNonce != tx.Nonce {
			return Response{
				Code: code.WrongNonce,
				Log:  fmt.Sprintf("Unexpected nonce. Expected: %d, got %d.", expectedNonce, tx.Nonce)}
		}
	}

	switch tx.Type {
	case TypeDeclareCandidacy:

		data := tx.GetDecodedData().(DeclareCandidacyData)

		if len(data.PubKey) != 32 {
			return Response{
				Code: code.IncorrectPubKey,
				Log:  fmt.Sprintf("Incorrect PubKey")}
		}

		commissionInBaseCoin := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commissionInBaseCoin.Mul(commissionInBaseCoin, CommissionMultiplier)
		commission := big.NewInt(0).Set(commissionInBaseCoin)

		if data.Coin != types.GetBaseCoin() {
			coin := context.GetStateCoin(data.Coin)

			if coin.ReserveBalance().Cmp(commissionInBaseCoin) < 0 {
				return Response{
					Code: code.CoinReserveNotSufficient,
					Log:  fmt.Sprintf("Coin reserve balance is not sufficient for transaction. Has: %s, required %s", coin.ReserveBalance().String(), commissionInBaseCoin.String())}
			}

			commission = formula.CalculateSaleAmount(coin.Volume(), coin.ReserveBalance(), coin.Data().Crr, commissionInBaseCoin)
		}

		totalTxCost := big.NewInt(0).Add(data.Stake, commission)

		if context.GetBalance(sender, data.Coin).Cmp(totalTxCost) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
		}

		if context.CandidateExists(data.PubKey) {
			return Response{
				Code: code.CandidateExists,
				Log:  fmt.Sprintf("Candidate with such public key (%x) already exists", data.PubKey)}
		}

		if data.Commission < minCommission || data.Commission > maxCommission {
			return Response{
				Code: code.WrongCommission,
				Log:  fmt.Sprintf("Commission should be between 0 and 100")}
		}

		// TODO: limit number of candidates to prevent flooding

		if !isCheck {
			rewardPull.Add(rewardPull, commission)

			context.SubBalance(sender, data.Coin, totalTxCost)
			context.CreateCandidate(data.Address, data.PubKey, data.Commission, uint(currentBlock), data.Coin, data.Stake)
			context.SetNonce(sender, tx.Nonce)
		}

		return Response{
			Code:      code.OK,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeSetCandidateOnline:

		data := tx.GetDecodedData().(SetCandidateOnData)

		commission := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commission.Mul(commission, CommissionMultiplier)

		if context.GetBalance(sender, types.GetBaseCoin()).Cmp(commission) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), commission)}
		}

		if !context.CandidateExists(data.PubKey) {
			return Response{
				Code: code.CandidateNotFound,
				Log:  fmt.Sprintf("Candidate with such public key (%x) not found", data.PubKey)}
		}

		candidate := context.GetStateCandidate(data.PubKey)

		if bytes.Compare(candidate.CandidateAddress.Bytes(), sender.Bytes()) != 0 {
			return Response{
				Code: code.IsNotOwnerOfCandidate,
				Log:  fmt.Sprintf("Sender is not an owner of a candidate")}
		}

		if !isCheck {
			rewardPull.Add(rewardPull, commission)

			context.SubBalance(sender, types.GetBaseCoin(), commission)
			context.SetCandidateOnline(data.PubKey)
			context.SetNonce(sender, tx.Nonce)
		}

		return Response{
			Code:      code.OK,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeSetCandidateOffline:

		data := tx.GetDecodedData().(SetCandidateOffData)

		commission := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commission.Mul(commission, CommissionMultiplier)

		if context.GetBalance(sender, types.GetBaseCoin()).Cmp(commission) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), commission)}
		}

		if !context.CandidateExists(data.PubKey) {
			return Response{
				Code: code.CandidateNotFound,
				Log:  fmt.Sprintf("Candidate with such public key not found")}
		}

		candidate := context.GetStateCandidate(data.PubKey)

		if bytes.Compare(candidate.CandidateAddress.Bytes(), sender.Bytes()) != 0 {
			return Response{
				Code: code.IsNotOwnerOfCandidate,
				Log:  fmt.Sprintf("Sender is not an owner of a candidate")}
		}

		if !isCheck {
			rewardPull.Add(rewardPull, commission)

			context.SubBalance(sender, types.GetBaseCoin(), commission)
			context.SetCandidateOffline(data.PubKey)
			context.SetNonce(sender, tx.Nonce)
		}

		return Response{
			Code:      code.OK,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeDelegate:

		data := tx.GetDecodedData().(DelegateData)

		commissionInBaseCoin := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commissionInBaseCoin.Mul(commissionInBaseCoin, CommissionMultiplier)
		commission := big.NewInt(0).Set(commissionInBaseCoin)

		if data.Coin != types.GetBaseCoin() {
			coin := context.GetStateCoin(data.Coin)

			if coin.ReserveBalance().Cmp(commissionInBaseCoin) < 0 {
				return Response{
					Code: code.CoinReserveNotSufficient,
					Log:  fmt.Sprintf("Coin reserve balance is not sufficient for transaction. Has: %s, required %s", coin.ReserveBalance().String(), commissionInBaseCoin.String())}
			}

			commission = formula.CalculateSaleAmount(coin.Volume(), coin.ReserveBalance(), coin.Data().Crr, commissionInBaseCoin)
		}

		totalTxCost := big.NewInt(0).Add(data.Stake, commission)

		if context.GetBalance(sender, data.Coin).Cmp(totalTxCost) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
		}

		if !context.CandidateExists(data.PubKey) {
			return Response{
				Code: code.CandidateNotFound,
				Log:  fmt.Sprintf("Candidate with such public key not found")}
		}

		if !isCheck {
			rewardPull.Add(rewardPull, commission)

			context.SubBalance(sender, data.Coin, totalTxCost)
			context.Delegate(sender, data.PubKey, data.Coin, data.Stake)
			context.SetNonce(sender, tx.Nonce)
		}

		return Response{
			Code:      code.OK,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeUnbond:

		data := tx.GetDecodedData().(UnbondData)

		commission := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commission.Mul(commission, CommissionMultiplier)

		if context.GetBalance(sender, types.GetBaseCoin()).Cmp(commission) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), commission)}
		}

		if !context.CandidateExists(data.PubKey) {
			return Response{
				Code: code.CandidateNotFound,
				Log:  fmt.Sprintf("Candidate with such public key not found")}
		}

		candidate := context.GetStateCandidate(data.PubKey)

		stake := candidate.GetStakeOfAddress(sender, data.Coin)

		if stake == nil {
			return Response{
				Code: code.StakeNotFound,
				Log:  fmt.Sprintf("Stake of current user not found")}
		}

		if stake.Value.Cmp(data.Value) < 0 {
			return Response{
				Code: code.InsufficientStake,
				Log:  fmt.Sprintf("Insufficient stake for sender account")}
		}

		if !isCheck {
			// now + 31 days
			unboundAtBlock := currentBlock + unboundPeriod

			rewardPull.Add(rewardPull, commission)

			context.SubBalance(sender, types.GetBaseCoin(), commission)
			context.SubStake(sender, data.PubKey, data.Coin, data.Value)
			context.GetOrNewStateFrozenFunds(unboundAtBlock).AddFund(sender, data.PubKey, data.Coin, data.Value)
			context.SetNonce(sender, tx.Nonce)
		}

		return Response{
			Code:      code.OK,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeSend:

		data := tx.GetDecodedData().(SendData)

		if !context.CoinExists(data.Coin) {
			return Response{
				Code: code.CoinNotExists,
				Log:  fmt.Sprintf("Coin not exists")}
		}

		commissionInBaseCoin := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commissionInBaseCoin.Mul(commissionInBaseCoin, CommissionMultiplier)
		commission := big.NewInt(0).Set(commissionInBaseCoin)

		if data.Coin != types.GetBaseCoin() {
			coin := context.GetStateCoin(data.Coin)

			if coin.ReserveBalance().Cmp(commissionInBaseCoin) < 0 {
				return Response{
					Code: code.CoinReserveNotSufficient,
					Log:  fmt.Sprintf("Coin reserve balance is not sufficient for transaction. Has: %s, required %s", coin.ReserveBalance().String(), commissionInBaseCoin.String())}
			}

			commission = formula.CalculateSaleAmount(coin.Volume(), coin.ReserveBalance(), coin.Data().Crr, commissionInBaseCoin)
		}

		totalTxCost := big.NewInt(0).Add(data.Value, commission)

		if context.GetBalance(sender, data.Coin).Cmp(totalTxCost) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
		}

		// deliver TX

		if !isCheck {
			rewardPull.Add(rewardPull, commissionInBaseCoin)

			if data.Coin != types.GetBaseCoin() {
				context.SubCoinVolume(data.Coin, commission)
				context.SubCoinReserve(data.Coin, commissionInBaseCoin)
			}

			context.SubBalance(sender, data.Coin, totalTxCost)
			context.AddBalance(data.To, data.Coin, data.Value)
			context.SetNonce(sender, tx.Nonce)
		}

		tags := common.KVPairs{
			common.KVPair{Key: []byte("tx.type"), Value: []byte{TypeSend}},
			common.KVPair{Key: []byte("tx.from"), Value: []byte(hex.EncodeToString(sender[:]))},
			common.KVPair{Key: []byte("tx.to"), Value: []byte(hex.EncodeToString(data.To[:]))},
			common.KVPair{Key: []byte("tx.coin"), Value: []byte(data.Coin.String())},
		}

		return Response{
			Code:      code.OK,
			Tags:      tags,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeRedeemCheck:

		data := tx.GetDecodedData().(RedeemCheckData)

		decodedCheck, err := check.DecodeFromBytes(data.RawCheck)

		if err != nil {
			return Response{
				Code: code.DecodeError,
				Log:  err.Error()}
		}

		checkSender, err := decodedCheck.Sender()

		if err != nil {
			return Response{
				Code: code.DecodeError,
				Log:  err.Error()}
		}

		if !context.CoinExists(decodedCheck.Coin) {
			return Response{
				Code: code.CoinNotExists,
				Log:  fmt.Sprintf("Coin not exists")}
		}

		if decodedCheck.DueBlock < uint64(currentBlock) {
			return Response{
				Code: code.CheckExpired,
				Log:  fmt.Sprintf("Check expired")}
		}

		if context.IsCheckUsed(decodedCheck) {
			return Response{
				Code: code.CheckUsed,
				Log:  fmt.Sprintf("Check already redeemed")}
		}

		// fixed potential problem with making too high commission for sender
		if tx.GasPrice.Cmp(big.NewInt(1)) == 1 {
			return Response{
				Code: code.TooHighGasPrice,
				Log:  fmt.Sprintf("Gas price for check is limited to 1")}
		}

		lockPublicKey, err := decodedCheck.LockPubKey()

		if err != nil {
			return Response{
				Code: code.DecodeError,
				Log:  err.Error()}
		}

		var senderAddressHash types.Hash
		hw := sha3.NewKeccak256()
		rlp.Encode(hw, []interface{}{
			sender,
		})
		hw.Sum(senderAddressHash[:0])

		pub, err := crypto.Ecrecover(senderAddressHash[:], data.Proof[:])

		if err != nil {
			return Response{
				Code: code.DecodeError,
				Log:  err.Error()}
		}

		if bytes.Compare(lockPublicKey, pub) != 0 {
			return Response{
				Code: code.CheckInvalidLock,
				Log:  fmt.Sprintf("Invalid proof")}
		}

		commissionInBaseCoin := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commissionInBaseCoin.Mul(commissionInBaseCoin, CommissionMultiplier)
		commission := big.NewInt(0).Set(commissionInBaseCoin)

		if decodedCheck.Coin != types.GetBaseCoin() {
			coin := context.GetStateCoin(decodedCheck.Coin)
			commission = formula.CalculateSaleAmount(coin.Volume(), coin.ReserveBalance(), coin.Data().Crr, commissionInBaseCoin)
		}

		totalTxCost := big.NewInt(0).Add(decodedCheck.Value, commission)

		if context.GetBalance(checkSender, decodedCheck.Coin).Cmp(totalTxCost) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for check issuer account: %s. Wanted %d ", checkSender.String(), totalTxCost)}
		}

		// deliver TX

		if !isCheck {
			context.UseCheck(decodedCheck)
			rewardPull.Add(rewardPull, commissionInBaseCoin)

			if decodedCheck.Coin != types.GetBaseCoin() {
				context.SubCoinVolume(decodedCheck.Coin, commission)
				context.SubCoinReserve(decodedCheck.Coin, commissionInBaseCoin)
			}

			context.SubBalance(checkSender, decodedCheck.Coin, totalTxCost)
			context.AddBalance(sender, decodedCheck.Coin, decodedCheck.Value)
			context.SetNonce(sender, tx.Nonce)
		}

		tags := common.KVPairs{
			common.KVPair{Key: []byte("tx.type"), Value: []byte{TypeRedeemCheck}},
			common.KVPair{Key: []byte("tx.from"), Value: []byte(hex.EncodeToString(checkSender[:]))},
			common.KVPair{Key: []byte("tx.to"), Value: []byte(hex.EncodeToString(sender[:]))},
			common.KVPair{Key: []byte("tx.coin"), Value: []byte(decodedCheck.Coin.String())},
		}

		return Response{
			Code:      code.OK,
			Tags:      tags,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeSellCoin:

		data := tx.GetDecodedData().(SellCoinData)

		if data.CoinToSell == data.CoinToBuy {
			return Response{
				Code: code.CrossConvert,
				Log:  fmt.Sprintf("\"From\" coin equals to \"to\" coin")}
		}

		if !context.CoinExists(data.CoinToSell) {
			return Response{
				Code: code.CoinNotExists,
				Log:  fmt.Sprintf("Coin not exists")}
		}

		if !context.CoinExists(data.CoinToBuy) {
			return Response{
				Code: code.CoinNotExists,
				Log:  fmt.Sprintf("Coin not exists")}
		}

		commissionInBaseCoin := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commissionInBaseCoin.Mul(commissionInBaseCoin, CommissionMultiplier)
		commission := big.NewInt(0).Set(commissionInBaseCoin)

		if data.CoinToSell != types.GetBaseCoin() {
			coin := context.GetStateCoin(data.CoinToSell)

			if coin.ReserveBalance().Cmp(commissionInBaseCoin) < 0 {
				return Response{
					Code: code.CoinReserveNotSufficient,
					Log:  fmt.Sprintf("Coin reserve balance is not sufficient for transaction. Has: %s, required %s", coin.ReserveBalance().String(), commissionInBaseCoin.String())}
			}

			commission = formula.CalculateSaleAmount(coin.Volume(), coin.ReserveBalance(), coin.Data().Crr, commissionInBaseCoin)
		}

		totalTxCost := big.NewInt(0).Add(data.ValueToSell, commission)

		if context.GetBalance(sender, data.CoinToSell).Cmp(totalTxCost) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
		}

		// deliver TX

		if !isCheck {
			rewardPull.Add(rewardPull, commissionInBaseCoin)

			context.SubBalance(sender, data.CoinToSell, totalTxCost)

			if data.CoinToSell != types.GetBaseCoin() {
				context.SubCoinVolume(data.CoinToSell, commission)
				context.SubCoinReserve(data.CoinToSell, commissionInBaseCoin)
			}
		}

		var value *big.Int

		if data.CoinToSell == types.GetBaseCoin() {
			coin := context.GetStateCoin(data.CoinToBuy).Data()

			value = formula.CalculatePurchaseReturn(coin.Volume, coin.ReserveBalance, coin.Crr, data.ValueToSell)

			if !isCheck {
				context.AddCoinVolume(data.CoinToBuy, value)
				context.AddCoinReserve(data.CoinToBuy, data.ValueToSell)
			}
		} else if data.CoinToBuy == types.GetBaseCoin() {
			coin := context.GetStateCoin(data.CoinToSell).Data()

			value = formula.CalculateSaleReturn(coin.Volume, coin.ReserveBalance, coin.Crr, data.ValueToSell)

			if !isCheck {
				context.SubCoinVolume(data.CoinToSell, data.ValueToSell)
				context.SubCoinReserve(data.CoinToSell, value)
			}
		} else {
			coinFrom := context.GetStateCoin(data.CoinToSell).Data()
			coinTo := context.GetStateCoin(data.CoinToBuy).Data()

			basecoinValue := formula.CalculateSaleReturn(coinFrom.Volume, coinFrom.ReserveBalance, coinFrom.Crr, data.ValueToSell)
			value = formula.CalculatePurchaseReturn(coinTo.Volume, coinTo.ReserveBalance, coinTo.Crr, basecoinValue)

			if !isCheck {
				context.AddCoinVolume(data.CoinToBuy, value)
				context.SubCoinVolume(data.CoinToSell, data.ValueToSell)

				context.AddCoinReserve(data.CoinToBuy, basecoinValue)
				context.SubCoinReserve(data.CoinToSell, basecoinValue)
			}
		}

		if !isCheck {
			context.AddBalance(sender, data.CoinToBuy, value)
			context.SetNonce(sender, tx.Nonce)
		}

		tags := common.KVPairs{
			common.KVPair{Key: []byte("tx.type"), Value: []byte{TypeSellCoin}},
			common.KVPair{Key: []byte("tx.from"), Value: []byte(hex.EncodeToString(sender[:]))},
			common.KVPair{Key: []byte("tx.coin_to_buy"), Value: []byte(data.CoinToBuy.String())},
			common.KVPair{Key: []byte("tx.coin_to_sell"), Value: []byte(data.CoinToSell.String())},
			common.KVPair{Key: []byte("tx.return"), Value: value.Bytes()},
		}

		return Response{
			Code:      code.OK,
			Tags:      tags,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeBuyCoin:

		data := tx.GetDecodedData().(BuyCoinData)

		if data.CoinToSell == data.CoinToBuy {
			return Response{
				Code: code.CrossConvert,
				Log:  fmt.Sprintf("\"From\" coin equals to \"to\" coin")}
		}

		if !context.CoinExists(data.CoinToSell) {
			return Response{
				Code: code.CoinNotExists,
				Log:  fmt.Sprintf("Coin not exists")}
		}

		if !context.CoinExists(data.CoinToBuy) {
			return Response{
				Code: code.CoinNotExists,
				Log:  fmt.Sprintf("Coin not exists")}
		}

		commissionInBaseCoin := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commissionInBaseCoin.Mul(commissionInBaseCoin, CommissionMultiplier)
		commission := big.NewInt(0).Set(commissionInBaseCoin)

		if data.CoinToSell != types.GetBaseCoin() {
			coin := context.GetStateCoin(data.CoinToSell)

			if coin.ReserveBalance().Cmp(commissionInBaseCoin) < 0 {
				return Response{
					Code: code.CoinReserveNotSufficient,
					Log:  fmt.Sprintf("Coin reserve balance is not sufficient for transaction. Has: %s, required %s", coin.ReserveBalance().String(), commissionInBaseCoin.String())}
			}

			commission = formula.CalculateSaleAmount(coin.Volume(), coin.ReserveBalance(), coin.Data().Crr, commissionInBaseCoin)
		}

		var value *big.Int

		if data.CoinToSell == types.GetBaseCoin() {
			coin := context.GetStateCoin(data.CoinToBuy).Data()

			value = formula.CalculatePurchaseAmount(coin.Volume, coin.ReserveBalance, coin.Crr, data.ValueToBuy)

			totalTxCost := big.NewInt(0).Add(value, commission)
			if context.GetBalance(sender, data.CoinToSell).Cmp(totalTxCost) < 0 {
				return Response{
					Code: code.InsufficientFunds,
					Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
			}

			if !isCheck {
				context.SubBalance(sender, data.CoinToSell, value)
				context.AddCoinVolume(data.CoinToBuy, data.ValueToBuy)
				context.AddCoinReserve(data.CoinToBuy, value)
			}
		} else if data.CoinToBuy == types.GetBaseCoin() {
			coin := context.GetStateCoin(data.CoinToSell).Data()

			value = formula.CalculateSaleAmount(coin.Volume, coin.ReserveBalance, coin.Crr, data.ValueToBuy)

			totalTxCost := big.NewInt(0).Add(value, commission)
			if context.GetBalance(sender, data.CoinToSell).Cmp(totalTxCost) < 0 {
				return Response{
					Code: code.InsufficientFunds,
					Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
			}

			if !isCheck {
				context.SubBalance(sender, data.CoinToSell, value)
				context.SubCoinVolume(data.CoinToSell, value)
				context.SubCoinReserve(data.CoinToSell, data.ValueToBuy)
			}
		} else {
			coinFrom := context.GetStateCoin(data.CoinToSell).Data()
			coinTo := context.GetStateCoin(data.CoinToBuy).Data()

			baseCoinNeeded := formula.CalculatePurchaseAmount(coinTo.Volume, coinTo.ReserveBalance, coinTo.Crr, data.ValueToBuy)
			value = formula.CalculateSaleAmount(coinFrom.Volume, coinFrom.ReserveBalance, coinFrom.Crr, baseCoinNeeded)

			totalTxCost := big.NewInt(0).Add(value, commission)
			if context.GetBalance(sender, data.CoinToSell).Cmp(totalTxCost) < 0 {
				return Response{
					Code: code.InsufficientFunds,
					Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
			}

			if !isCheck {
				context.SubBalance(sender, data.CoinToSell, value)

				context.AddCoinVolume(data.CoinToBuy, data.ValueToBuy)
				context.SubCoinVolume(data.CoinToSell, value)

				context.AddCoinReserve(data.CoinToBuy, baseCoinNeeded)
				context.SubCoinReserve(data.CoinToSell, baseCoinNeeded)
			}
		}

		if !isCheck {
			rewardPull.Add(rewardPull, commissionInBaseCoin)

			context.SubBalance(sender, data.CoinToSell, commission)

			if data.CoinToSell != types.GetBaseCoin() {
				context.SubCoinVolume(data.CoinToSell, commission)
				context.SubCoinReserve(data.CoinToSell, commissionInBaseCoin)
			}

			context.AddBalance(sender, data.CoinToBuy, value)
			context.SetNonce(sender, tx.Nonce)
		}

		tags := common.KVPairs{
			common.KVPair{Key: []byte("tx.type"), Value: []byte{TypeBuyCoin}},
			common.KVPair{Key: []byte("tx.from"), Value: []byte(hex.EncodeToString(sender[:]))},
			common.KVPair{Key: []byte("tx.coin_to_buy"), Value: []byte(data.CoinToBuy.String())},
			common.KVPair{Key: []byte("tx.coin_to_sell"), Value: []byte(data.CoinToSell.String())},
			common.KVPair{Key: []byte("tx.return"), Value: value.Bytes()},
		}

		return Response{
			Code:      code.OK,
			Tags:      tags,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	case TypeCreateCoin:

		data := tx.GetDecodedData().(CreateCoinData)

		if match, _ := regexp.MatchString(allowedCoinSymbols, data.Symbol.String()); !match {
			return Response{
				Code: code.InvalidCoinSymbol,
				Log:  fmt.Sprintf("Invalid coin symbol. Should be %s", allowedCoinSymbols)}
		}

		commission := big.NewInt(0).Mul(tx.GasPrice, big.NewInt(tx.Gas()))
		commission.Mul(commission, CommissionMultiplier)

		// compute additional price from letters count
		lettersCount := len(data.Symbol.String())
		var price int64 = 0
		switch lettersCount {
		case 3:
			price += 1000000 // 1mln bips
		case 4:
			price += 100000 // 100k bips
		case 5:
			price += 10000 // 10k bips
		case 6:
			price += 1000 // 1k bips
		case 7:
			price += 100 // 100 bips
		case 8:
			price += 10 // 10 bips
		}
		p := big.NewInt(10)
		p.Exp(p, big.NewInt(18), nil)
		p.Mul(p, big.NewInt(price))
		commission.Add(commission, p)

		totalTxCost := big.NewInt(0).Add(data.InitialReserve, commission)

		if context.GetBalance(sender, types.GetBaseCoin()).Cmp(totalTxCost) < 0 {
			return Response{
				Code: code.InsufficientFunds,
				Log:  fmt.Sprintf("Insufficient funds for sender account: %s. Wanted %d ", sender.String(), totalTxCost)}
		}

		if context.CoinExists(data.Symbol) {
			return Response{
				Code: code.CoinAlreadyExists,
				Log:  fmt.Sprintf("Coin already exists")}
		}

		if data.ConstantReserveRatio < 10 || data.ConstantReserveRatio > 100 {
			return Response{
				Code: code.WrongCrr,
				Log:  fmt.Sprintf("Constant Reserve Ratio should be between 10 and 100")}
		}

		// deliver TX

		if !isCheck {
			rewardPull.Add(rewardPull, commission)

			context.SubBalance(sender, types.GetBaseCoin(), totalTxCost)
			context.CreateCoin(data.Symbol, data.Name, data.InitialAmount, data.ConstantReserveRatio, data.InitialReserve, sender)
			context.AddBalance(sender, data.Symbol, data.InitialAmount)
			context.SetNonce(sender, tx.Nonce)
		}

		tags := common.KVPairs{
			common.KVPair{Key: []byte("tx.type"), Value: []byte{TypeCreateCoin}},
			common.KVPair{Key: []byte("tx.from"), Value: []byte(hex.EncodeToString(sender[:]))},
			common.KVPair{Key: []byte("tx.coin"), Value: []byte(data.Symbol.String())},
		}

		return Response{
			Code:      code.OK,
			Tags:      tags,
			GasUsed:   tx.Gas(),
			GasWanted: tx.Gas(),
		}
	default:
		return Response{Code: code.UnknownTransactionType}
	}

	return Response{Code: code.UnknownTransactionType}
}
