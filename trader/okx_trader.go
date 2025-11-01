package trader

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/okx/okx-api-v5-sdk/okxapi/client"
	"github.com/okx/okx-api-v5-sdk/okxapi/models/account"
	"github.com/okx/okx-api-v5-sdk/okxapi/models/market"
	"github.com/okx/okx-api-v5-sdk/okxapi/models/public"
	"github.com/okx/okx-api-v5-sdk/okxapi/models/trade"
	"github.com/okx/okx-api-v5-sdk/okxapi/rest"
)

// OKX API URL
const (
	OKXProductionURL = "https.www.okx.com"
	OKXDemoURL       = "https.www.okx.com" // OKX的模拟盘URL
)

// OkxTrader OKX 交易平台实现
type OkxTrader struct {
	client     *client.Client
	ctx        context.Context
	testnet    bool
	precisions sync.Map // 缓存精度信息 map[string]int
}

// NewOkxTrader 创建OKX交易器
func NewOkxTrader(apiKey, secretKey, passphrase string, testnet bool) (*OkxTrader, error) {
	var dest rest.Destination
	if testnet {
		dest = rest.Demo
	} else {
		// OKX 针对不同地区有不同服务器
		// AWS (aws.okx.com), AWS-Speed (aws-speed.okx.com), GCP (gcp.okx.com)
		// 我们使用默认的 rest.Aws
		dest = rest.Aws
	}

	cli, err := client.New(
		context.Background(),
		apiKey,
		secretKey,
		passphrase,
		dest,
	)
	if err != nil {
		return nil, fmt.Errorf("创建OKX客户端失败: %w", err)
	}

	// 尝试获取时间以验证API连接
	if _, err := cli.Rest.Api.Public.GetTime(); err != nil {
		return nil, fmt.Errorf("连接OKX API失败 (请检查API密钥、Passphrase或网络): %w", err)
	}

	log.Printf("✓ OKX交易器初始化成功 (testnet=%v)", testnet)

	return &OkxTrader{
		client:  cli,
		ctx:     context.Background(),
		testnet: testnet,
	}, nil
}

// --- 助手函数 ---

// okxSymbol 将 "BTCUSDT" 转换为 "BTC-USDT-SWAP"
func okxSymbol(symbol string) string {
	if strings.HasSuffix(symbol, "USDT") {
		return strings.Replace(symbol, "USDT", "-USDT-SWAP", 1)
	}
	return symbol + "-USDT-SWAP"
}

// standardSymbol 将 "BTC-USDT-SWAP" 转换为 "BTCUSDT"
func standardSymbol(instID string) string {
	if strings.HasSuffix(instID, "-USDT-SWAP") {
		return strings.Replace(instID, "-USDT-SWAP", "USDT", 1)
	}
	return instID
}

// parseFloat 辅助解析字符串
func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// parseInt 辅助解析字符串
func parseInt(s string) int {
	i, _ := strconv.ParseInt(s, 10, 64)
	return int(i)
}

// --- 实现 trader.Trader 接口 ---

func (t *OkxTrader) GetBalance() (map[string]interface{}, error) {
	log.Printf("🔄 正在调用OKX API获取账户余额...")
	// OKX V5 GetAccount API
	resp, err := t.client.Rest.Api.Account.GetAccount(&account.GetAccountRequest{
		Ccy: "USDT",
	})
	if err != nil {
		return nil, fmt.Errorf("OKX GetBalance 失败: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("OKX GetBalance: 未返回账户数据")
	}

	acc := resp.Data[0]

	// 映射到标准格式
	totalEq := parseFloat(acc.TotalEq)
	upl := parseFloat(acc.Upl)
	availEq := parseFloat(acc.AvailEq)

	// totalWalletBalance = 账户净值 - 未实现盈亏
	totalWalletBalance := totalEq - upl

	result := map[string]interface{}{
		"totalWalletBalance":    totalWalletBalance,
		"availableBalance":      availEq,
		"totalUnrealizedProfit": upl,
	}

	return result, nil
}

func (t *OkxTrader) GetPositions() ([]map[string]interface{}, error) {
	log.Printf("🔄 正在调用OKX API获取持仓信息...")
	resp, err := t.client.Rest.Api.Account.GetPositions(&account.GetPositionsRequest{
		InstType: "SWAP", // 只获取永续合约
	})
	if err != nil {
		return nil, fmt.Errorf("OKX GetPositions 失败: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range resp.Data {
		posAmt := parseFloat(pos.Pos)
		if posAmt == 0 {
			continue // 跳过空仓位
		}

		// 转换 symbol 格式
		symbol := standardSymbol(pos.InstID)

		posMap := make(map[string]interface{})
		posMap["symbol"] = symbol
		posMap["side"] = pos.PosSide // "long" or "short"
		posMap["positionAmt"] = posAmt
		posMap["entryPrice"] = parseFloat(pos.AvgPx)
		posMap["markPrice"] = parseFloat(pos.MarkPx)
		posMap["unRealizedProfit"] = parseFloat(pos.Upl)
		posMap["leverage"] = parseFloat(pos.Lever)
		posMap["liquidationPrice"] = parseFloat(pos.LiqPx)

		result = append(result, posMap)
	}

	return result, nil
}

func (t *OkxTrader) SetLeverage(symbol string, leverage int) error {
	instID := okxSymbol(symbol)
	log.Printf("🔄 正在调用OKX API设置杠杆 for %s to %dx", instID, leverage)

	// OKX需要同时设置多空杠杆（如果posSide不填）
	req := &account.SetLeverageRequest{
		InstID:  instID,
		Lever:   fmt.Sprintf("%d", leverage),
		MgnMode: "isolated", // 必须设为逐仓
	}

	_, err := t.client.Rest.Api.Account.SetLeverage(req)
	if err != nil {
		// 忽略 "Leverage not change" 错误
		if strings.Contains(err.Error(), "Leverage not change") {
			log.Printf("  ✓ %s 杠杆已是 %dx，无需切换", instID, leverage)
			return nil
		}
		return fmt.Errorf("OKX SetLeverage 失败: %w", err)
	}

	log.Printf("  ✓ %s 杠杆已切换为 %dx", instID, leverage)
	// OKX API 延迟
	time.Sleep(500 * time.Millisecond)
	return nil
}

// 内部函数：下单
func (t *OkxTrader) placeOrder(symbol, side, ordType, posSide string, quantity float64) (map[string]interface{}, error) {
	instID := okxSymbol(symbol)
	
	// 格式化数量
	quantityStr, err := t.FormatQuantity(instID, quantity)
	if err != nil {
		return nil, err
	}
	
	req := &trade.PlaceOrderRequest{
		InstID:  instID,
		TdMode:  "isolated", // 逐仓
		Side:    side,
		OrdType: ordType,
		Sz:      quantityStr,
	}

	// 如果是平仓，需要指定 posSide
	if posSide != "" {
		req.PosSide = posSide
	}

	resp, err := t.client.Rest.Api.Trade.PlaceOrder(req)
	if err != nil {
		return nil, fmt.Errorf("OKX PlaceOrder 失败 (%s %s %s): %w", instID, side, quantityStr, err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("OKX PlaceOrder 未返回订单数据")
	}

	orderData := resp.Data[0]
	if orderData.SCode != "0" {
		return nil, fmt.Errorf("OKX 下单失败: %s (code: %s)", orderData.SMsg, orderData.SCode)
	}

	result := make(map[string]interface{})
	result["orderId"] = orderData.OrdID
	result["symbol"] = symbol
	result["status"] = "FILLED" // 市价单假定立即成交

	return result, nil
}

func (t *OkxTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	log.Printf("📈 正在调用OKX API开多仓: %s, 数量: %f", symbol, quantity)
	
	if err := t.CancelAllOrders(symbol); err != nil {
		log.Printf("  ⚠ 取消旧委托单失败: %v", err)
	}
	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, err
	}
	
	return t.placeOrder(symbol, "buy", "market", "long", quantity)
}

func (t *OkxTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	log.Printf("📉 正在调用OKX API开空仓: %s, 数量: %f", symbol, quantity)

	if err := t.CancelAllOrders(symbol); err != nil {
		log.Printf("  ⚠ 取消旧委托单失败: %v", err)
	}
	if err := t.SetLeverage(symbol, leverage); err != nil {
		return nil, err
	}

	return t.placeOrder(symbol, "sell", "market", "short", quantity)
}

func (t *OkxTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	log.Printf("🔄 正在调用OKX API平多仓: %s, 数量: %f", symbol, quantity)

	if quantity == 0 {
		// 获取当前持仓量
		pos, err := t.getSpecificPosition(symbol, "long")
		if err != nil {
			return nil, err
		}
		if pos == nil {
			return nil, fmt.Errorf("没有找到 %s 的多仓", symbol)
		}
		quantity = parseFloat(pos.Pos)
	}

	return t.placeOrder(symbol, "sell", "market", "long", quantity)
}

func (t *OkxTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	log.Printf("🔄 正在调用OKX API平空仓: %s, 数量: %f", symbol, quantity)

	if quantity == 0 {
		// 获取当前持仓量
		pos, err := t.getSpecificPosition(symbol, "short")
		if err != nil {
			return nil, err
		}
		if pos == nil {
			return nil, fmt.Errorf("没有找到 %s 的空仓", symbol)
		}
		quantity = parseFloat(pos.Pos)
	}

	return t.placeOrder(symbol, "buy", "market", "short", quantity)
}

func (t *OkxTrader) getSpecificPosition(symbol, posSide string) (*account.Position, error) {
	instID := okxSymbol(symbol)
	resp, err := t.client.Rest.Api.Account.GetPositions(&account.GetPositionsRequest{
		InstID: instID,
	})
	if err != nil {
		return nil, err
	}
	for _, pos := range resp.Data {
		if pos.PosSide == posSide {
			return pos, nil
		}
	}
	return nil, nil // 未找到
}

func (t *OkxTrader) GetMarketPrice(symbol string) (float64, error) {
	instID := okxSymbol(symbol)
	resp, err := t.client.Rest.Api.Market.GetTicker(&market.GetTickerRequest{
		InstID: instID,
	})
	if err != nil {
		return 0, fmt.Errorf("OKX GetTicker 失败: %w", err)
	}
	if len(resp.Data) == 0 {
		return 0, fmt.Errorf("OKX GetTicker: 未返回 %s 的数据", instID)
	}
	return parseFloat(resp.Data[0].Last), nil
}

// 内部函数：设置止损/止盈
func (t *OkxTrader) placeAlgoOrder(symbol, posSide, ordType, triggerPrice, sz string) error {
	instID := okxSymbol(symbol)
	
	side := "sell" // 平多
	if posSide == "short" {
		side = "buy" // 平空
	}

	req := &trade.PlaceAlgoOrderRequest{
		InstID:  instID,
		TdMode:  "isolated",
		Side:    side,
		PosSide: posSide,
		OrdType: ordType,
		Sz:      sz,
	}

	if ordType == "stop" {
		req.SlTriggerPx = triggerPrice
		req.SlOrdPx = "-1" // 市价止损
	} else if ordType == "tp" {
		req.TpTriggerPx = triggerPrice
		req.TpOrdPx = "-1" // 市价止盈
	}

	resp, err := t.client.Rest.Api.Trade.PlaceAlgoOrder(req)
	if err != nil {
		return fmt.Errorf("OKX PlaceAlgoOrder 失败: %w", err)
	}
	if len(resp.Data) == 0 {
		return fmt.Errorf("OKX PlaceAlgoOrder 未返回数据")
	}
	if resp.Data[0].SCode != "0" {
		return fmt.Errorf("OKX PlaceAlgoOrder 失败: %s (code: %s)", resp.Data[0].SMsg, resp.Data[0].SCode)
	}
	return nil
}

func (t *OkxTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	log.Printf("🛡️ 正在调用OKX API设置止损: %s, 价格: %f", symbol, stopPrice)
	
	// OKX的 positionSide 是 "long" or "short"
	posSide := strings.ToLower(positionSide) 
	quantityStr, err := t.FormatQuantity(okxSymbol(symbol), quantity)
	if err != nil {
		return err
	}
	stopPriceStr, _ := t.FormatPrice(okxSymbol(symbol), stopPrice) // 止损价也需要精度

	return t.placeAlgoOrder(symbol, posSide, "stop", stopPriceStr, quantityStr)
}

func (t *OkxTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	log.Printf("💰 正在调用OKX API设置止盈: %s, 价格: %f", symbol, takeProfitPrice)
	
	posSide := strings.ToLower(positionSide)
	quantityStr, err := t.FormatQuantity(okxSymbol(symbol), quantity)
	if err != nil {
		return err
	}
	tpPriceStr, _ := t.FormatPrice(okxSymbol(symbol), takeProfitPrice) // 止盈价也需要精度

	return t.placeAlgoOrder(symbol, posSide, "tp", tpPriceStr, quantityStr)
}

func (t *OkxTrader) CancelAllOrders(symbol string) error {
	instID := okxSymbol(symbol)
	log.Printf("🚫 正在调用OKX API取消所有订单: %s", instID)

	// 1. 取消所有普通订单
	// (OKX似乎没有批量取消特定symbol的接口，需要先获取再取消，或者直接取消所有)
	// 这里我们用CancelMultipleOrders，但需要订单ID，这不符合"CancelAll"
	// 更好的办法是取消所有策略订单
	
	// 2. 取消所有策略订单（止损/止盈）
	// (同样，没有批量取消特定symbol的接口，需要先获取)
	
	// 简化：获取所有未成交的策略订单并取消
	algoList, err := t.client.Rest.Api.Trade.GetAlgoOrderList(&trade.GetAlgoOrderListRequest{
		InstType: "SWAP",
		InstID:   instID,
		OrdType:  "stop", // 止损
	})
	if err == nil {
		for _, algo := range algoList.Data {
			t.client.Rest.Api.Trade.CancelAlgoOrder(&trade.CancelAlgoOrderRequest{
				InstID: instID,
				AlgoID: algo.AlgoID,
			})
		}
	}
	
	algoList, err = t.client.Rest.Api.Trade.GetAlgoOrderList(&trade.GetAlgoOrderListRequest{
		InstType: "SWAP",
		InstID:   instID,
		OrdType:  "tp", // 止盈
	})
	if err == nil {
		for _, algo := range algoList.Data {
			t.client.Rest.Api.Trade.CancelAlgoOrder(&trade.CancelAlgoOrderRequest{
				InstID: instID,
				AlgoID: algo.AlgoID,
			})
		}
	}

	return nil
}

// getInstrument 获取合约信息（用于精度）
func (t *OkxTrader) getInstrument(instID string) (*public.Instrument, error) {
	resp, err := t.client.Rest.Api.Public.GetInstruments(&public.GetInstrumentsRequest{
		InstType: "SWAP",
		InstID:   instID,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("未找到合约信息: %s", instID)
	}
	return &resp.Data[0], nil
}

// getLotSzPrecision 获取数量精度
func (t *OkxTrader) getLotSzPrecision(instID string) (int, error) {
	if val, ok := t.precisions.Load(instID + "_lotSz"); ok {
		return val.(int), nil
	}

	inst, err := t.getInstrument(instID)
	if err != nil {
		return 0, err
	}
	
	// lotSz 是最小下单张数，我们需要的是 "ctVal"
	// OKX 合约单位是 "张" (cont), 数量 (sz) 必须是 "ctVal" 的整数倍
	// 对于USDT保证金合约，ctVal通常是 0.1 (ETH), 0.01 (BTC)
	// 我们需要的是 "lotSz"（最小下单数量）
	
	precision := calculatePrecision(inst.LotSz)
	t.precisions.Store(instID+"_lotSz", precision)
	return precision, nil
}

// getTickSzPrecision 获取价格精度
func (t *OkxTrader) getTickSzPrecision(instID string) (int, error) {
	if val, ok := t.precisions.Load(instID + "_tickSz"); ok {
		return val.(int), nil
	}
	inst, err := t.getInstrument(instID)
	if err != nil {
		return 0, err
	}
	precision := calculatePrecision(inst.TickSz)
	t.precisions.Store(instID+"_tickSz", precision)
	return precision, nil
}

func (t *OkxTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	instID := okxSymbol(symbol)
	precision, err := t.getLotSzPrecision(instID)
	if err != nil {
		// 默认精度
		log.Printf("⚠ %s 未找到数量精度，使用默认精度3", instID)
		precision = 3
	}
	
	format := fmt.Sprintf("%%.%df", precision)
	return fmt.Sprintf(format, quantity), nil
}

func (t *OkxTrader) FormatPrice(symbol string, price float64) (string, error) {
	instID := okxSymbol(symbol)
	precision, err := t.getTickSzPrecision(instID)
	if err != nil {
		// 默认精度
		log.Printf("⚠ %s 未找到价格精度，使用默认精度2", instID)
		precision = 2
	}

	format := fmt.Sprintf("%%.%df", precision)
	return fmt.Sprintf(format, price), nil
}
