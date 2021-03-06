package engine

import (
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	orderidLayout = "2006-01-02|15:04:05.000"
)

type CoreStrategyChannels struct {
	errors    chan error
	events    chan event
	portfolio chan *PortfolioNewPositionEvent
}

func (c *CoreStrategyChannels) isValid() bool {
	if c.errors == nil {
		return false
	}
	if c.events == nil {
		return false
	}
	if c.portfolio == nil {
		return false
	}
	return true
}

type ICoreStrategy interface {
	init(ch CoreStrategyChannels)
	ticks() TickArray
	candles() CandleArray
	setPortfolio(p *portfolioHandler)
	enableEventLogging()
	notify(e event)
	shutDown()
	getInstrument() *Instrument
}

type IUserStrategy interface {
	OnTick(b *BasicStrategy, tick *Tick)
	OnCandleClose(b *BasicStrategy, candle *Candle)
	OnCandleOpen(b *BasicStrategy, price float64)
}

type BasicStrategy struct {
	portfolio *portfolioHandler
	isReady   bool
	symbol    *Instrument
	nPeriods  int

	ch                         CoreStrategyChannels
	terminationChan            chan struct{}
	waitingConfirmation        map[string]struct{}
	waitingN                   int32
	closedTrades               []*Trade
	currentTrade               *Trade
	Ticks                      TickArray
	Candles                    CandleArray
	lastCandleOpen             float64
	lastCandleOpenTime         time.Time
	userStrategy               IUserStrategy
	mostRecentTime             time.Time
	mut                        *sync.Mutex
	isEventLoggingEnabled      bool
	isEventSliceStorageEnabled bool

	log                log.Logger
	eventsLoggingSlice eventsSliceStorage
	mdChan             chan event
	handlersWaitGroup  *sync.WaitGroup
}

//******* Connection methods ***********************

func (b *BasicStrategy) shutDown() {
	b.handlersWaitGroup.Wait()
}

func (b *BasicStrategy) getInstrument() *Instrument{
	return b.symbol
}
func (b *BasicStrategy) init(ch CoreStrategyChannels) {
	if !ch.isValid() {
		panic("Core chans are not valid. Some of them is nil")
	}

	b.ch = ch
	b.terminationChan = make(chan struct{})
	b.waitingConfirmation = make(map[string]struct{})
	b.mut = &sync.Mutex{}

	if b.currentTrade == nil {
		b.currentTrade = newFlatTrade(b.symbol)
	}
	if len(b.closedTrades) == 0 {
		b.closedTrades = []*Trade{}
	}

	b.isReady = true

}

func (b *BasicStrategy) setPortfolio(p *portfolioHandler) {
	b.portfolio = p
}

//*******API CALLS************************************************
func (b *BasicStrategy) GetTotalPnL() float64 {
	return b.portfolio.totalPnL()
}

func (b *BasicStrategy) OpenOrders() map[string]*Order {
	return b.currentTrade.ConfirmedOrders
}

func (b *BasicStrategy) Position() int64 {
	if b.currentTrade.Type == FlatTrade || b.currentTrade.Type == ClosedTrade {
		return 0
	}
	pos := b.currentTrade.Qty
	if b.currentTrade.Type == ShortTrade {
		pos = -pos
	}

	return pos

}

func (b *BasicStrategy) IsOrderConfirmed(ordId string) bool {
	return b.currentTrade.hasConfirmedOrderWithId(ordId)
}

func (b *BasicStrategy) OrderStatus(ordId string) OrderState {
	//Todo
	return ""
}

func (b *BasicStrategy) NewLimitOrder(price float64, side OrderSide, qty int64, tif OrderTIF, destination string) (string, error) {
	order := Order{
		Side:        side,
		Qty:         qty,
		Ticker:      b.symbol,
		Price:       price,
		State:       NewOrder,
		Type:        LimitOrder,
		Tif:         tif,
		Destination: destination,
		Time:        b.mostRecentTime.Add(20 * time.Microsecond),
		Id:          fmt.Sprintf("%v_%v_%v", price, LimitOrder, rand.Float64()),
	}

	err := b.newOrder(&order)
	return order.Id, err

}

func (b *BasicStrategy) NewMarketOrder(side OrderSide, qty int64, tif OrderTIF, destination string) (string, error) {
	order := Order{
		Side:        side,
		Qty:         qty,
		Ticker:      b.symbol,
		Price:       math.NaN(),
		State:       NewOrder,
		Type:        MarketOrder,
		Tif:         tif,
		Destination: destination,
		Time:        b.mostRecentTime,
		Id:          fmt.Sprintf("%v_%v", MarketOrder, rand.Float64()),
	}

	err := b.newOrder(&order)
	return order.Id, err

}

func (b *BasicStrategy) CancelOrder(ordID string) error {
	//fmt.Println("Cancel order")
	if ordID == "" {
		err := ErrOrderIdIncorrect{
			OrdId:   ordID,
			Message: "Id is empty. ",
			Caller:  "CancelOrder func",
		}
		return &err
	}

	if !b.currentTrade.hasConfirmedOrderWithId(ordID) {
		err := ErrOrderNotFoundInConfirmedMap{
			ErrOrderNotFoundInOrdersMap{
				OrdId:   ordID,
				Message: "Id is empty. ",
				Caller:  "CancelOrder func",
			},
		}
		return &err
	}

	cancelReq := OrderCancelRequestEvent{
		OrdId:     ordID,
		BaseEvent: be(b.mostRecentTime.Add(20*time.Microsecond), b.currentTrade.ConfirmedOrders[ordID].Ticker),
	}

	reqID := "$CAN$" + ordID
	if _, ok := b.waitingConfirmation[reqID]; ok {
		return errors.New("Request is already waiting for conf. ")
	} else {
		b.waitingConfirmation[reqID] = struct{}{}
		atomic.AddInt32(&b.waitingN, 1)
	}

	b.newSignal(&cancelReq)

	return nil
}

func (b *BasicStrategy) ReplaceOrder(ordID string, newPrice float64) error {
	if ordID == "" {
		err := ErrOrderIdIncorrect{
			OrdId:   ordID,
			Message: "Id is empty. ",
			Caller:  "ReplaceOrder func",
		}
		return &err
	}

	if !b.currentTrade.hasConfirmedOrderWithId(ordID) {
		err := ErrOrderNotFoundInConfirmedMap{
			ErrOrderNotFoundInOrdersMap{
				OrdId:   ordID,
				Message: "Id is empty. ",
				Caller:  "ReplaceOrder func",
			},
		}
		return &err
	}

	replaceReq := OrderReplaceRequestEvent{
		OrdId:     ordID,
		NewPrice:  newPrice,
		BaseEvent: be(b.mostRecentTime.Add(20*time.Microsecond), b.symbol),
	}

	reqID := "$REP$" + ordID
	if _, ok := b.waitingConfirmation[reqID]; ok {
		return errors.New("Request is already waiting for conf. ")
	} else {
		b.waitingConfirmation[reqID] = struct{}{}
		atomic.AddInt32(&b.waitingN, 1)
	}

	b.newSignal(&replaceReq)

	return nil
}

func (b *BasicStrategy) LastCandleOpen() float64 {
	return b.lastCandleOpen
}

//****** MARKET DATA AND EVENT PROCESSORS ******************************************

func (b *BasicStrategy) notify(e event) {
	switch e.(type) {
	case *NewTickEvent:
		b.proxyEvent(e)
	default:
		b.sendEventForLogging(e)
		b.proxyEvent(e)
	}

}

func (b *BasicStrategy) proxyEvent(e event) {
	switch i := e.(type) {
	case *OrderCancelEvent:
		b.onOrderCancelHandler(i)
	case *OrderCancelRejectEvent:
		b.onOrderCancelRejectHandler(i)
	case *OrderConfirmationEvent:
		b.onOrderConfirmHandler(i)
	case *OrderReplacedEvent:
		b.onOrderReplacedHandler(i)
	case *OrderReplaceRejectEvent:
		b.onOrderReplaceRejectHandler(i)
	case *OrderRejectedEvent:
		b.onOrderRejectedHandler(i)
	case *OrderFillEvent:
		b.onOrderFillHandler(i)
	case *StrategyRequestNotDeliveredEvent:
		b.onStrategyRequestNotDeliveredEventHandler(i)
	case *NewTickEvent:
		b.onTickHandler(i)
	case *TickHistoryEvent:
		b.onTickHistoryHandler(i)
	case *CandleCloseEvent:
		b.onCandleCloseHandler(i)
	case *CandleOpenEvent:
		b.onCandleOpenHandler(i)
	case *EndOfDataEvent:
		b.onEndOfDataHandler(i)

	default:
		panic("Unexpected event type in BasicStrategy: " + e.getName())
	}
}

//****** EVENT HANDLERS *******************************************************

func (b *BasicStrategy) onCandleCloseHandler(e *CandleCloseEvent) {
	<-b.mdChan
	b.handlersWaitGroup.Add(1)
	go func() {
		defer func() {
			b.handlersWaitGroup.Done()
			b.mdChan <- e

		}()

		if e == nil {
			return
		}

		b.mut.Lock()
		defer b.mut.Unlock()

		if e.getTime().After(b.mostRecentTime) {
			b.mostRecentTime = e.getTime()
		}

		if !e.Candle.isValid() {
			return
		}

		b.putNewCandle(e.Candle)

		if b.currentTrade.IsOpen() {
			err := b.currentTrade.updatePnL(e.Candle.Close, e.Candle.Datetime)
			if err != nil {
				b.newError(err)
			}
		}
		if len(b.Candles) < b.nPeriods {

			return
		}

		b.userStrategy.OnCandleClose(b, e.Candle)
	}()

}

func (b *BasicStrategy) onCandleOpenHandler(e *CandleOpenEvent) {
	<-b.mdChan
	b.handlersWaitGroup.Add(1)
	go func() {
		defer func() {
			b.handlersWaitGroup.Done()
			b.mdChan <- e

		}()

		if e == nil {
			return
		}

		b.mut.Lock()
		defer b.mut.Unlock()

		if e.CandleTime.After(b.mostRecentTime) {
			b.mostRecentTime = e.CandleTime
		}

		if !e.CandleTime.Before(b.lastCandleOpenTime) {
			b.lastCandleOpen = e.Price
			b.lastCandleOpenTime = e.CandleTime
		}
		if b.currentTrade.IsOpen() {
			err := b.currentTrade.updatePnL(e.Price, e.CandleTime)
			if err != nil {
				b.newError(err)
			}
		}

		b.userStrategy.OnCandleOpen(b, e.Price)

	}()

}

//onCandleHistoryHandler puts historical candles in current array of candles.
func (b *BasicStrategy) onCandleHistoryHandler(e *CandlesHistoryEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()

	if e.Candles == nil {
		return
	}
	if len(e.Candles) == 0 {
		return
	}

	allCandles := append(b.Candles, e.Candles...)
	listedCandleTimes := make(map[time.Time]struct{})
	var checkedCandles CandleArray

	for _, v := range allCandles {
		if v == nil {
			continue
		}
		if !v.isValid() {
			continue
		}
		if _, ok := listedCandleTimes[v.Datetime]; ok {
			continue
		}

		checkedCandles = append(checkedCandles, v)
		listedCandleTimes[v.Datetime] = struct{}{}
	}

	sort.SliceStable(checkedCandles, func(i, j int) bool {
		return checkedCandles[i].Datetime.Unix() < checkedCandles[j].Datetime.Unix()
	})

	if len(checkedCandles) > b.nPeriods {
		b.Candles = checkedCandles[len(checkedCandles)-b.nPeriods:]
	} else {
		b.Candles = checkedCandles
	}

	b.updateLastCandleOpen()

	return
}

func (b *BasicStrategy) onTickHandler(e *NewTickEvent) {
	<-b.mdChan
	b.handlersWaitGroup.Add(1)
	go func() {

		defer func() {
			b.handlersWaitGroup.Done()
			b.mdChan <- e

		}()

		if e == nil {
			return
		}
		if !e.Tick.IsValid() {
			return
		}

		/*if e.Tick.Datetime.Sub(b.prevTick.Datetime) > time.Duration(1*time.Second) {
				for atomic.LoadInt32(&b.waitingN) > 0 {
					//runtime.Gosched()

				}
			} todo*/

		b.mut.Lock()
		defer b.mut.Unlock()

		if e.Tick.Datetime.After(b.mostRecentTime) {
			b.mostRecentTime = e.Tick.Datetime
		}

		b.putNewTick(e.Tick)
		if b.currentTrade.IsOpen() {
			err := b.currentTrade.updatePnL(e.Tick.LastPrice, e.Tick.Datetime)
			if err != nil {
				b.newError(err)
			}
		}
		if len(b.Ticks) < b.nPeriods {
			return
		}

		b.userStrategy.OnTick(b, e.Tick)
		b.sendEventForLogging(e)
	}()

}

//onTickHistoryHandler puts history ticks in current array of ticks. It doesn't produce any events.
func (b *BasicStrategy) onTickHistoryHandler(e *TickHistoryEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()

	if e.Ticks == nil {
		return
	}

	if len(e.Ticks) == 0 {
		return
	}

	allTicks := append(b.Ticks, e.Ticks...)

	var checkedTicks TickArray

	for _, v := range allTicks {
		if v == nil {
			continue
		}
		if !v.IsValid() {
			continue
		}

		checkedTicks = append(checkedTicks, v)

	}

	sort.SliceStable(checkedTicks, func(i, j int) bool {
		return checkedTicks[i].Datetime.Unix() < checkedTicks[j].Datetime.Unix()
	})

	if len(checkedTicks) > b.nPeriods {
		b.Ticks = checkedTicks[len(checkedTicks)-b.nPeriods:]
	} else {
		b.Ticks = checkedTicks
	}

	return
}

func (b *BasicStrategy) onOrderFillHandler(e *OrderFillEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()

	if e.getTime().After(b.mostRecentTime) {
		b.mostRecentTime = e.getTime()
	}

	if e.Ticker != b.symbol {
		b.newError(errors.New("Mismatch symbols in fill event and position. "))
	}

	if e.Qty <= 0 {
		b.newError(errors.New("Execution Qty is zero or less. "))
	}

	if math.IsNaN(e.Price) || e.Price <= 0 {
		b.newError(errors.New("Price is NaN or less or equal to zero. "))
	}

	prevState := b.currentTrade.Type
	newPos, err := b.currentTrade.executeOrder(e.OrdId, e.Qty, e.Price, e.Time)

	if err != nil {
		b.newError(err)
		return
	}
	if newPos != nil {
		if b.currentTrade.Type != ClosedTrade {
			b.newError(errors.New("New position opened, but previous is not closed. "))
			return
		}
		b.closedTrades = append(b.closedTrades, b.currentTrade)
		b.currentTrade = newPos
		//fmt.Println("New trade to portf event")
		b.notifyPortfolioAboutPosition(&PortfolioNewPositionEvent{be(e.getTime(), b.symbol), b.currentTrade})

	} else {
		if prevState == FlatTrade {
			//fmt.Println("New trade to portf event")
			b.notifyPortfolioAboutPosition(&PortfolioNewPositionEvent{be(e.getTime(), b.symbol), b.currentTrade})
		}
	}

}

func (b *BasicStrategy) onOrderCancelHandler(e *OrderCancelEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()

	if e.getTime().After(b.mostRecentTime) {
		b.mostRecentTime = e.getTime()
	}

	atomic.AddInt32(&b.waitingN, -1)
	delete(b.waitingConfirmation, "&CAN&"+e.OrdId)

	err := b.currentTrade.cancelOrder(e.OrdId)

	if err != nil {
		b.newError(err)
		return
	}

}

func (b *BasicStrategy) onStrategyRequestNotDeliveredEventHandler(e *StrategyRequestNotDeliveredEvent) {
	if e.Request == nil {
		b.newError(errors.New("onStrategyRequestNotDeliveredEventHandler got event with nil Request field"))
		return
	}

	panic("Not implemented") // Todo
}

func (b *BasicStrategy) onOrderCancelRejectHandler(e *OrderCancelRejectEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()

	if e.getTime().After(b.mostRecentTime) {
		b.mostRecentTime = e.getTime()
	}

	atomic.AddInt32(&b.waitingN, -1)
	delete(b.waitingConfirmation, "&CAN&"+e.OrdId)

}

func (b *BasicStrategy) onOrderReplaceRejectHandler(e *OrderReplaceRejectEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()

	if e.getTime().After(b.mostRecentTime) {
		b.mostRecentTime = e.getTime()
	}

	atomic.AddInt32(&b.waitingN, -1)
	delete(b.waitingConfirmation, "&REP&"+e.OrdId)

}

func (b *BasicStrategy) onOrderConfirmHandler(e *OrderConfirmationEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()

	if e.getTime().After(b.mostRecentTime) {
		b.mostRecentTime = e.getTime()
	}

	atomic.AddInt32(&b.waitingN, -1)
	delete(b.waitingConfirmation, "&NO&"+e.OrdId)

	err := b.currentTrade.confirmOrder(e.OrdId)

	if err != nil {
		b.newError(err)
		return
	}
}

func (b *BasicStrategy) onOrderReplacedHandler(e *OrderReplacedEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()
	if e.getTime().After(b.mostRecentTime) {
		b.mostRecentTime = e.getTime()
	}

	atomic.AddInt32(&b.waitingN, -1)
	delete(b.waitingConfirmation, "&REP&"+e.OrdId)

	err := b.currentTrade.replaceOrder(e.OrdId, e.NewPrice)

	if err != nil {
		b.newError(err)
	}

}

func (b *BasicStrategy) onOrderRejectedHandler(e *OrderRejectedEvent) {
	b.mut.Lock()
	defer b.mut.Unlock()
	if e.getTime().After(b.mostRecentTime) {
		b.mostRecentTime = e.getTime()
	}

	atomic.AddInt32(&b.waitingN, -1)
	delete(b.waitingConfirmation, "&NO&"+e.OrdId)

	err := b.currentTrade.rejectOrder(e.OrdId, e.Reason)

	if err != nil {
		b.newError(err)
		return
	}
}

func (b *BasicStrategy) onEndOfDataHandler(e *EndOfDataEvent) {
	b.terminationChan <- struct{}{}
}

//Private funcs to work with data
func (b *BasicStrategy) newError(err error) {
	b.handlersWaitGroup.Add(1)
	go func() {
		b.ch.errors <- err
		b.handlersWaitGroup.Done()
	}()

}

func (b *BasicStrategy) newSignal(e event) {
	b.sendEventForLogging(e)
	b.ch.events <- e
}

func (b *BasicStrategy) newOrder(order *Order) error {
	if order.Ticker != b.symbol {
		return errors.New("Can't put new order. Strategy symbol and order symbol are different. ")
	}
	if order.Id == "" {
		order.Id = order.Time.Format(orderidLayout)
	}

	if !order.isValid() {
		return errors.New("Order is not valid. ")
	}
	order.Id = b.symbol.Symbol + "|" + string(order.Side) + "|" + order.Id

	err := b.currentTrade.putNewOrder(order)

	if err != nil {
		b.newError(err)
		return err
	}
	ordEvent := NewOrderEvent{
		LinkedOrder: order,
		BaseEvent:   be(b.mostRecentTime, order.Ticker),
	}

	reqID := "$NO$" + order.Id
	if _, ok := b.waitingConfirmation[reqID]; ok {
		return errors.New("Order is already waiting for conf. ")
	} else {
		b.waitingConfirmation[reqID] = struct{}{}
		atomic.AddInt32(&b.waitingN, 1)
	}
	b.newSignal(&ordEvent)
	return nil
}

func (b *BasicStrategy) notifyPortfolioAboutPosition(e *PortfolioNewPositionEvent) {
	b.handlersWaitGroup.Add(1)
	go func() {
		b.ch.portfolio <- e
		b.handlersWaitGroup.Done()
	}()
}

func (b *BasicStrategy) enableEventLogging() {
	pth := path.Join("./StrategyLogs", b.symbol.Symbol+".txt")
	f, err := os.OpenFile(pth, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		panic(err)
	}
	b.log.SetOutput(f)
	b.isEventLoggingEnabled = true
}

func (b *BasicStrategy) enableEventSliceStorage() {
	b.isEventSliceStorageEnabled = true
	b.eventsLoggingSlice = eventsSliceStorage{mut: &sync.Mutex{}}
}

func (b *BasicStrategy) sendEventForLogging(e event) {
	if b.isEventLoggingEnabled {
		message := fmt.Sprintf("[SE:%v]  %+v", b.symbol, e.String())
		b.log.Print(message)
	}
	if b.isEventSliceStorageEnabled {
		b.eventsLoggingSlice.add(e)
	}
}

func (b *BasicStrategy) putNewCandle(candle *Candle) {
	if candle == nil {
		return
	}

	sortIt := false
	if len(b.Candles) > 0 && candle.Datetime.Before(b.Candles[len(b.Candles)-1].Datetime) {
		sortIt = true
	}

	if len(b.Candles) < b.nPeriods {
		b.Candles = append(b.Candles, candle)
		b.updateLastCandleOpen()
		return
	}
	b.Candles = append(b.Candles[1:], candle)

	if sortIt {
		sort.SliceStable(b.Candles, func(i, j int) bool {
			return b.Candles[i].Datetime.Unix() < b.Candles[j].Datetime.Unix()
		})
	}

	b.updateLastCandleOpen()
	return
}

func (b *BasicStrategy) putNewTick(tick *Tick) {
	if tick == nil {
		return
	}
	sortIt := false
	if len(b.Ticks) > 0 && tick.Datetime.Before(b.Ticks[len(b.Ticks)-1].Datetime) {
		sortIt = true
	}

	if len(b.Ticks) < b.nPeriods {
		b.Ticks = append(b.Ticks, tick)
		return
	}
	b.Ticks = append(b.Ticks[1:], tick)

	if sortIt {
		sort.SliceStable(b.Ticks, func(i, j int) bool {
			return b.Ticks[i].Datetime.Unix() < b.Ticks[j].Datetime.Unix()
		})
	}
	return
}

func (b *BasicStrategy) updateLastCandleOpen() {
	if len(b.Candles) == 0 {
		return
	}
	lastCandleInHist := b.Candles[len(b.Candles)-1]
	if lastCandleInHist.Datetime.After(b.lastCandleOpenTime) {
		b.lastCandleOpen = lastCandleInHist.Open
		b.lastCandleOpenTime = lastCandleInHist.Datetime
	}

}

func (b *BasicStrategy) ticks() TickArray {
	return b.Ticks
}

func (b *BasicStrategy) candles() CandleArray {
	return b.Candles
}




