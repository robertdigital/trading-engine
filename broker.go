package engine

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

type eventArray []event

func (t eventArray) sort() {
	sort.SliceStable(t, func(i, j int) bool {
		return t[i].getTime().Unix() < t[j].getTime().Unix()
	})
}

type IBroker interface {
	Connect()
	Disconnect()
	Init(errChan chan error, events chan event, symbols []*Instrument)
	IsSimulated() bool
	Notify(e event)
	shutDown()
}

type simBrokerOrder struct {
	*Order
	BrokerState   OrderState
	StateUpdTime  time.Time
	BrokerExecQty int64
	BrokerPrice   float64
}

func (o *simBrokerOrder) getExpirationTime() time.Time {
	switch o.Tif {
	case GTCTIF:
		return o.Time.AddDate(10, 0, 0)
	case DayTIF:
		nextDay := o.Time.AddDate(0, 0, 1)
		return time.Date(nextDay.Year(), nextDay.Month(), nextDay.Day(), 0, 0, 0, 0, o.Time.Location())
	case AuctionTIF:
		if o.Type == MarketOnOpen || o.Type == LimitOnOpen {
			mot := o.Ticker.Exchange.MarketOpenTime
			t := time.Date(o.Time.Year(), o.Time.Month(), o.Time.Day(), mot.Hour, mot.Minute, mot.Second, 0, o.Time.Location())
			return t.Add(3 * time.Minute)
		}

		if o.Type == MarketOnClose || o.Type == LimitOnClose {
			mct := o.Ticker.Exchange.MarketCloseTime
			t := time.Date(o.Time.Year(), o.Time.Month(), o.Time.Day(), mct.Hour, mct.Minute, mct.Second, 0, o.Time.Location())
			return t.Add(3 * time.Second)
		}

		panic("Found non auction order type with auction tif")
	default:
		panic("Unknown order tif: " + string(o.Tif))
	}
}

func (o *simBrokerOrder) isExpired(t time.Time) bool {
	if t.After(o.getExpirationTime()) {
		return true
	}
	return false
}

func (o *simBrokerOrder) isActive() bool {
	if o.BrokerState == ConfirmedOrder || o.BrokerState == PartialFilledOrder {
		return true
	}

	return false
}

type SimBroker struct {
	delay                  int64
	checkExecutionsOnTicks bool
	strictLimitOrders      bool
	workers                map[string]*simBrokerWorker
}

func (b *SimBroker) Connect() {
	fmt.Println("SimBroker connected")
}

func (b *SimBroker) Disconnect() {
	fmt.Println("SimBroker connected")
}

func (b *SimBroker) Init(errChan chan error, events chan event, symbols []*Instrument) {
	if len(symbols) == 0 {
		panic("No symbols specified")
	}
	b.workers = make(map[string]*simBrokerWorker)

	for _, s := range symbols {
		bw := simBrokerWorker{
			symbol:            s,
			errChan:           errChan,
			events:            events,
			delay:             b.delay,
			strictLimitOrders: b.strictLimitOrders,
			mpMutext:          &sync.RWMutex{},
			waitGroup:         &sync.WaitGroup{},
			orders:            make(map[string]*simBrokerOrder),
		}
		b.workers[s.Symbol] = &bw

	}
}

func (b *SimBroker) Notify(e event) {
	w := b.workers[e.getSymbol()]
	w.notify(e)
}

func (b *SimBroker) shutDown() {
	for _, w := range b.workers {
		w.shutDown()
	}
}

func (b *SimBroker) IsSimulated() bool {
	return true
}

// $$$$$$$$$ SIM BROKER WORKER $$$$$$$$$$$$$$$$
type simBrokerWorker struct {
	symbol            *Instrument
	errChan           chan error
	events            chan event
	delay             int64
	strictLimitOrders bool

	mpMutext        *sync.RWMutex
	orders          map[string]*simBrokerOrder
	generatedEvents eventArray
	requestEvents   eventArray
	waitGroup       *sync.WaitGroup
	lastTickTime    time.Time
	lastCandleTime  time.Time
}

func (b *simBrokerWorker) notify(e event) {
	switch i := e.(type) {
	case *NewOrderEvent:
		b.addRequestEvent(e)
	case *OrderCancelRequestEvent:
		b.addRequestEvent(e)
	case *OrderReplaceRequestEvent:
		b.addRequestEvent(e)
	case *NewTickEvent:
		b.onTick(i)
	case *CandleOpenEvent:
		b.onCandleOpen(i)
	case *CandleCloseEvent:
		b.onCandleClose(i)
	default:
		panic("Unexpected event type in broker: " + e.getName())
	}
}

func (b *simBrokerWorker) addRequestEvent(e event) {
	b.requestEvents = append(b.requestEvents, e)
}

func (b *simBrokerWorker) genTimeRoundTrip(baseTime time.Time) time.Time {
	newEvTime := baseTime.Add(time.Duration(b.delay*2) * time.Millisecond)
	return newEvTime
}

func (b *simBrokerWorker) genTimeSingleTrip(baseTime time.Time) time.Time {
	newEvTime := baseTime.Add(time.Duration(b.delay) * time.Millisecond)
	return newEvTime
}

func (b *simBrokerWorker) validateOrderForExecution(order *simBrokerOrder, expectedType OrderType) error {
	if !order.isValid() {
		err := ErrInvalidOrder{
			OrdId:   order.Id,
			Message: fmt.Sprintf("Order : %+v", order),
			Caller:  "Sim Broker",
		}

		return &err
	}

	if order.Type != expectedType {
		err := ErrUnexpectedOrderType{
			OrdId:        order.Id,
			ActualType:   string(order.Type),
			ExpectedType: string(expectedType),
			Message:      "Got in fillOnTickLimit",
			Caller:       "Sim Broker",
		}
		return &err
	}

	if order.BrokerState != ConfirmedOrder && order.BrokerState != PartialFilledOrder {

		err := ErrUnexpectedOrderState{
			OrdId:         order.Id,
			ActualState:   string(order.State),
			ExpectedState: string(ConfirmedOrder) + "," + string(PartialFilledOrder),
			Message:       "Got in fillOnTickLimit",
			Caller:        "Sim Broker",
		}
		return &err
	}

	lvsQty := order.Qty - order.BrokerExecQty
	if lvsQty <= 0 {
		return errors.New("Sim broker: Lvs qty is zero or less. Nothing to execute. ")
	}

	return nil
}

func (b *simBrokerWorker) addBrokerEvent(e event) {

	switch i := e.(type) {

	case *OrderConfirmationEvent:
		ord, ok := b.orders[i.OrdId]
		if !ok {
			panic("Confirmation of not existing order")
		}
		ord.BrokerState = ConfirmedOrder
		ord.StateUpdTime = e.getTime()

	case *OrderCancelRejectEvent:

	case *OrderReplaceRejectEvent:

	case *OrderCancelEvent:
		ord, ok := b.orders[i.OrdId]
		if !ok {
			msg := fmt.Sprintf("Can't find order %v in confirmed map to cancel it. ", i.OrdId)
			panic(msg)
		}

		ord.BrokerState = CanceledOrder
		ord.StateUpdTime = e.getTime()

	case *OrderReplacedEvent:
		ord, ok := b.orders[i.OrdId]
		if !ok {
			msg := fmt.Sprintf("Can't find order %v in confirmed map to replace it. ", i.OrdId)
			panic(msg)
		}

		ord.StateUpdTime = e.getTime()
		ord.BrokerPrice = i.NewPrice

	case *OrderFillEvent:
		ord, ok := b.orders[i.OrdId]
		if !ok {
			msg := fmt.Sprintf("Can't find order %v in confirmed map to fill it. ", i.OrdId)
			panic(msg)
		}

		execQty := i.Qty

		if execQty == ord.Qty-ord.BrokerExecQty {
			ord.BrokerState = FilledOrder
		} else {
			if execQty > ord.Qty-ord.BrokerExecQty {
				panic("Large qty")
			}
			ord.BrokerState = PartialFilledOrder
		}

		ord.BrokerExecQty += i.Qty

	case *OrderRejectedEvent:
		ord, ok := b.orders[i.OrdId]
		if !ok {
			panic("Confirmation of not existing order")
		}
		if ord.BrokerState != ConfirmedOrder {
			ord.BrokerState = RejectedOrder
		}

		ord.StateUpdTime = e.getTime()

	}

	b.generatedEvents = append(b.generatedEvents, e)

}

func (b *simBrokerWorker) newError(e error) {
	if b.errChan == nil {
		panic("Simulated broker error chan is nil")
	}
	b.waitGroup.Add(1)
	go func() {
		b.errChan <- e
		b.waitGroup.Done()
	}()

}

func (b *simBrokerWorker) shutDown() {
	b.waitGroup.Wait()
}

//********* EVENT HANDLERS **********************************************************

func (b *simBrokerWorker) onNewOrder(e *NewOrderEvent) {
	b.mpMutext.Lock()
	defer b.mpMutext.Unlock()

	if !e.LinkedOrder.isValid() {
		r := "Sim Broker: can't confirm order. Order is not valid"
		rejectEvent := OrderRejectedEvent{
			OrdId:     e.LinkedOrder.Id,
			Reason:    r,
			BaseEvent: be(b.genTimeRoundTrip(e.getTime()), e.Ticker),
		}
		b.orders[e.LinkedOrder.Id] = &simBrokerOrder{
			Order:         e.LinkedOrder,
			BrokerState:   RejectedOrder,
			BrokerExecQty: 0,
			StateUpdTime:  rejectEvent.getTime(),
		}
		b.addBrokerEvent(&rejectEvent)
		return
	}

	if _, ok := b.orders[e.LinkedOrder.Id]; ok {
		r := "Sim Broker: can't confirm order. Order with this ID already exists on broker side"
		rejectEvent := OrderRejectedEvent{
			OrdId:     e.LinkedOrder.Id,
			Reason:    r,
			BaseEvent: be(b.genTimeRoundTrip(e.getTime()), e.Ticker),
		}
		b.addBrokerEvent(&rejectEvent)

		return
	}

	confEvent := OrderConfirmationEvent{
		OrdId:     e.LinkedOrder.Id,
		BaseEvent: be(b.genTimeRoundTrip(e.getTime()), e.Ticker),
	}

	b.orders[e.LinkedOrder.Id] = &simBrokerOrder{
		Order:         e.LinkedOrder,
		BrokerState:   NewOrder,
		BrokerExecQty: 0,
		BrokerPrice:   e.LinkedOrder.Price,
		StateUpdTime:  confEvent.getTime(),
	}

	b.addBrokerEvent(&confEvent)

}

func (b *simBrokerWorker) onCancelRequest(e *OrderCancelRequestEvent) {
	b.mpMutext.Lock()
	defer b.mpMutext.Unlock()
	newEvTime := b.genTimeRoundTrip(e.getTime())

	if _, ok := b.orders[e.OrdId]; !ok {
		e := OrderCancelRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Broker can't find order with ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	if b.orders[e.OrdId].BrokerState == CanceledOrder {
		e := OrderCancelRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Order is already canceled ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	if b.orders[e.OrdId].BrokerState == NewOrder {
		e := OrderCancelRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Order is not confirmed yet ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	if b.orders[e.OrdId].BrokerState == FilledOrder {
		e := OrderCancelRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Order is already filled ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	orderCancelE := OrderCancelEvent{
		OrdId:     e.OrdId,
		BaseEvent: be(newEvTime, e.Ticker),
	}
	b.addBrokerEvent(&orderCancelE)

}

func (b *simBrokerWorker) onReplaceRequest(e *OrderReplaceRequestEvent) {
	b.mpMutext.Lock()
	defer b.mpMutext.Unlock()

	newEvTime := b.genTimeRoundTrip(e.getTime())

	if _, ok := b.orders[e.OrdId]; !ok {
		e := OrderReplaceRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Broker can't find order with ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	if b.orders[e.OrdId].BrokerState == CanceledOrder {
		e := OrderReplaceRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Order is already canceled ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	if b.orders[e.OrdId].BrokerState == NewOrder {
		e := OrderReplaceRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Order is not confirmed yet ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	if b.orders[e.OrdId].BrokerState == FilledOrder {
		e := OrderReplaceRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    "Order is already filled ID: " + e.OrdId,
		}
		b.addBrokerEvent(&e)
		return
	}

	if math.IsNaN(e.NewPrice) || e.NewPrice == 0 {
		e := OrderReplaceRejectEvent{
			BaseEvent: be(newEvTime, e.Ticker),
			OrdId:     e.OrdId,
			Reason:    fmt.Sprintf("Replace price %v is not valid", e.NewPrice),
		}
		b.addBrokerEvent(&e)
		return
	}

	replacedEvent := OrderReplacedEvent{
		OrdId:     e.OrdId,
		NewPrice:  e.NewPrice,
		BaseEvent: be(newEvTime, e.Ticker),
	}
	b.addBrokerEvent(&replacedEvent)
}

func (b *simBrokerWorker) onCandleOpen(e *CandleOpenEvent) {
	if e.CandleTime.Before(b.lastCandleTime) {
		panic("Candle before seen candle")
	}
	b.lastCandleTime = e.CandleTime
	b.proceedStoredRequests(e.getTime())
	b.findExecutions(e)
}

func (b *simBrokerWorker) onCandleClose(e *CandleCloseEvent) {
	if e.getTime().Before(b.lastCandleTime) {
		panic("Candle before seen candle")
	}
	b.lastCandleTime = e.getTime()
	b.proceedStoredRequests(e.getTime())
	b.findExecutions(e)
}

func (b *simBrokerWorker) onTick(e *NewTickEvent) {
	if !e.Tick.IsValid() {
		err := ErrBrokenTick{
			Tick:    *e.Tick,
			Message: "Got in onTick",
			Caller:  "Sim Broker",
		}
		b.newError(&err)
		return
	}
	if e.Tick.Datetime.Before(b.lastTickTime) {
		panic("Tick before seen tick")
	}
	b.lastTickTime = e.Tick.Datetime
	b.proceedStoredRequests(e.getTime())
	b.findExecutions(e)

}

// ************ ORDER EXECUTORS *************************************************************

func (b *simBrokerWorker) cancelByTif(o *simBrokerOrder, t time.Time) bool {
	if !o.isExpired(t) {
		return false
	}

	e := OrderCancelEvent{
		BaseEvent: be(o.getExpirationTime(), o.Ticker),
		OrdId:     o.Id,
	}
	b.addBrokerEvent(&e)
	return true
}

func (b *simBrokerWorker) proceedStoredRequests(beforeTime time.Time) {
	if len(b.requestEvents) == 0 {
		return
	}
	b.requestEvents.sort()
	var eventsLeft eventArray
	for _, e := range b.requestEvents {
		if b.genTimeSingleTrip(e.getTime()).Before(beforeTime) {
			switch i := e.(type) {
			case *NewOrderEvent:
				b.onNewOrder(i)
			case *OrderCancelRequestEvent:
				b.onCancelRequest(i)
			case *OrderReplaceRequestEvent:
				b.onReplaceRequest(i)
			default:
				panic("Unexpected event type")
			}
		} else {
			eventsLeft = append(eventsLeft, e)
		}
	}

	b.requestEvents = eventsLeft
}

func (b *simBrokerWorker) findExecutions(mdEvent event) {
	b.mpMutext.Lock()
	defer b.mpMutext.Unlock()

	if len(b.orders) == 0 && len(b.generatedEvents) == 0 {
		b.events <- mdEvent
		return
	}

	b.generatedEvents = append(b.generatedEvents, mdEvent)

	var genEvents []event

	switch i := mdEvent.(type) {
	case *NewTickEvent:
		for _, o := range b.orders {
			if o.isActive() && o.Ticker.Symbol == i.Ticker.Symbol {
				if o.StateUpdTime.Before(i.Tick.Datetime) {
					cancel := b.cancelByTif(o, i.Tick.Datetime)
					if cancel {
						continue
					}
					e := b.findExecutionsOnTick(o, i.Tick)
					if e != nil {
						genEvents = append(genEvents, e...)
					}

				}
			}
		}
	case *CandleCloseEvent:
		for _, o := range b.orders {
			if o.Ticker.Symbol == i.Candle.Ticker.Symbol && o.isActive() {
				if o.StateUpdTime.Before(i.getTime()) {
					cancel := b.cancelByTif(o, i.Candle.Datetime)
					if cancel {
						continue
					}
					e := b.findExecutionsOnCandleClose(o, i)
					if e != nil {
						genEvents = append(genEvents, e)
					}
				}
			}
		}
	case *CandleOpenEvent:
		for _, o := range b.orders {
			if o.Ticker == i.Ticker && o.isActive() {
				if o.StateUpdTime.Before(i.CandleTime) {
					cancel := b.cancelByTif(o, i.CandleTime)
					if cancel {
						continue
					}
					e := b.findExecutionsOnCandleOpen(o, i)
					if e != nil {
						genEvents = append(genEvents, e)
					}
				} else {
					if (o.Type == MarketOrder || o.Type == StopOrder) && !o.StateUpdTime.After(i.getTime()) {
						e := b.findExecutionsOnCandleOpen(o, i)
						if e != nil {
							genEvents = append(genEvents, e)
						}
					}
				}
			}
		}
	default:
		panic("Unexpected event type for simBrokerWorker")

	}

	if len(genEvents) > 0 {
		for _, e := range genEvents {
			b.addBrokerEvent(e)
		}
	}

	b.generatedEvents.sort()

	var eventsLeft eventArray
	var eventsToSend eventArray

	for _, e := range b.generatedEvents {
		if !e.getTime().After(mdEvent.getTime()) {
			eventsToSend = append(eventsToSend, e)
		} else {
			eventsLeft = append(eventsLeft, e)
		}
	}

	b.generatedEvents = eventsLeft

	for _, e := range eventsToSend {
		b.events <- e
	}

}

func (b *simBrokerWorker) findExecutionsOnCandleClose(o *simBrokerOrder, e *CandleCloseEvent) event {
	switch o.Type {
	case LimitOrder:
		return b.fillOnCandleCloseLimit(o, e)
	case StopOrder:
		return b.fillOnCandleCloseStop(o, e)
	case LimitOnClose:
		return b.fillOnCandleCloseLOC(o, e)
	case MarketOnClose:
		return b.fillOnCandleCloseMOC(o, e)
	case MarketOrder:
		panic("Not implemented")
	}

	return nil

}

func (b *simBrokerWorker) findExecutionsOnCandleOpen(o *simBrokerOrder, e *CandleOpenEvent) event {

	switch o.Type {
	case LimitOrder:
		return b.fillOnCandleOpenLimit(o, e)
	case LimitOnOpen:
		return b.fillOnCandleOpenLOO(o, e)
	case StopOrder:
		return b.fillOnCandleOpenStop(o, e)
	case MarketOnOpen:
		return b.fillOnCandleOpenMOO(o, e)
	case MarketOrder:
		return b.fillOnCandleOpenMarket(o, e)
	}

	return nil
}

func (b *simBrokerWorker) findExecutionsOnTick(orderSim *simBrokerOrder, tick *Tick) []event {

	err := b.validateOrderForExecution(orderSim, orderSim.Type)
	if err != nil {
		b.newError(err)
		return nil
	}

	convertToList := func(e event) []event {
		if e == nil {
			return nil
		}
		return []event{e}
	}

	switch orderSim.Type {

	case MarketOrder:
		e := convertToList(b.fillOnTickMarket(orderSim, tick))
		return e
	case LimitOrder:
		e := convertToList(b.fillOnTickLimit(orderSim, tick))
		return e
	case StopOrder:
		e := convertToList(b.fillOnTickStop(orderSim, tick))
		return e
	case LimitOnClose:
		e := b.fillOnTickLOC(orderSim, tick)
		return e
	case LimitOnOpen:
		e := b.fillOnTickLOO(orderSim, tick)
		return e
	case MarketOnOpen:
		e := convertToList(b.fillOnTickMOO(orderSim, tick))
		return e
	case MarketOnClose:
		e := convertToList(b.fillOnTickMOC(orderSim, tick))
		return e
	default:
		err := ErrUnknownOrderType{
			OrdId:   orderSim.Id,
			Message: "found order with type: " + string(orderSim.Type),
			Caller:  "Sim Broker",
		}
		b.newError(&err)
	}

	return nil

}

// ********* CANDLE CLOSE EXECUTORS *********************************************************

func (b *simBrokerWorker) fillOnCandleCloseLimit(o *simBrokerOrder, e *CandleCloseEvent) event {
	c := e.Candle
	fillPrice := math.NaN()
	if !o.StateUpdTime.Before(c.Datetime) {
		if o.Side == OrderBuy && c.Low < c.Open && c.Low < o.BrokerPrice {
			fillPrice = o.BrokerPrice
		}

		if o.Side == OrderSell && c.High > c.Open && c.High > o.BrokerPrice {
			fillPrice = o.BrokerPrice
		}
	} else {
		switch o.Side {
		case OrderBuy:
			if (c.Low < o.BrokerPrice) || (c.Low == o.BrokerPrice && !b.strictLimitOrders) {

				if (e.TimeFrame == "D" || e.TimeFrame == "W" || (e.TimeFrame != "D" && c.isOpening())) && c.Open < o.BrokerPrice {
					fillPrice = c.Open
				} else {
					fillPrice = o.BrokerPrice
				}
			}
		case OrderSell:
			if (c.High > o.BrokerPrice) || (c.High == o.BrokerPrice && !b.strictLimitOrders) {

				if (e.TimeFrame == "D" || e.TimeFrame == "W" || (e.TimeFrame != "D" && c.isOpening())) && c.Open > o.BrokerPrice {
					fillPrice = c.Open
				} else {
					fillPrice = o.BrokerPrice
				}
			}
		default:
			panic("Unknown order side: " + string(o.Side))

		}
	}

	if math.IsNaN(fillPrice) {
		return nil
	}

	fe := OrderFillEvent{
		BaseEvent: be(e.getTime(), e.Ticker),
		OrdId:     o.Id,
		Price:     fillPrice,
		Qty:       o.Qty - o.BrokerExecQty,
	}

	return &fe
}

func (b *simBrokerWorker) fillOnCandleCloseMOC(o *simBrokerOrder, e *CandleCloseEvent) event {
	if !e.Candle.isClosingForTimeFrame(e.TimeFrame) {
		return nil
	}

	fe := OrderFillEvent{
		BaseEvent: be(e.getTime(), e.Ticker),
		OrdId:     o.Id,
		Price:     e.Candle.Close,
		Qty:       o.Qty - o.BrokerExecQty,
	}

	return &fe

}

func (b *simBrokerWorker) fillOnCandleCloseLOC(o *simBrokerOrder, e *CandleCloseEvent) event {
	if !e.Candle.isClosingForTimeFrame(e.TimeFrame) {
		return nil
	}
	fillPrice := math.NaN()
	switch o.Side {
	case OrderBuy:
		if e.Candle.Close < o.BrokerPrice || (e.Candle.Close == o.BrokerPrice && !b.strictLimitOrders) {
			fillPrice = e.Candle.Close
		}

	case OrderSell:
		if e.Candle.Close > o.BrokerPrice || (e.Candle.Close == o.BrokerPrice && !b.strictLimitOrders) {
			fillPrice = e.Candle.Close
		}

	default:
		panic("Unknown side")

	}

	if math.IsNaN(fillPrice) {
		cancelE := OrderCancelEvent{
			BaseEvent: be(e.getTime(), e.Ticker),
			OrdId:     o.Id,
		}
		return &cancelE
	}

	fe := OrderFillEvent{
		BaseEvent: be(e.getTime(), e.Ticker),
		OrdId:     o.Id,
		Price:     fillPrice,
		Qty:       o.Qty - o.BrokerExecQty,
	}

	return &fe

}

func (b *simBrokerWorker) fillOnCandleCloseStop(o *simBrokerOrder, e *CandleCloseEvent) event {
	c := e.Candle
	fillPrice := math.NaN()
	if !o.StateUpdTime.Before(c.Datetime) {
		if o.Side == OrderBuy && c.High > c.Open && c.High > o.BrokerPrice {
			fillPrice = c.Open
		}

		if o.Side == OrderSell && c.Low < c.Open && c.Low < o.BrokerPrice {
			fillPrice = c.Open
		}
	} else {
		switch o.Side {
		case OrderBuy:
			if c.High >= o.BrokerPrice {
				if c.Open > o.BrokerPrice {
					fillPrice = c.Open
				} else {
					fillPrice = o.BrokerPrice
				}
			}
		case OrderSell:
			if c.Low <= o.BrokerPrice {
				if c.Open < o.BrokerPrice {
					fillPrice = c.Open
				} else {
					fillPrice = o.BrokerPrice
				}
			}
		default:
			panic("Unknown order side: " + string(o.Side))

		}
	}

	if math.IsNaN(fillPrice) {
		return nil
	}

	fe := OrderFillEvent{
		BaseEvent: be(e.getTime(), e.Ticker),
		OrdId:     o.Id,
		Price:     fillPrice,
		Qty:       o.Qty - o.BrokerExecQty,
	}

	return &fe
}

// ********* CANDLE OPEN EXECUTORS ***********************************************************

func (b *simBrokerWorker) fillOnCandleOpenMOO(o *simBrokerOrder, e *CandleOpenEvent) event {
	if e.TimeFrame == "D" || e.TimeFrame == "W" {
		fe := OrderFillEvent{
			BaseEvent: be(e.getTime(), e.Ticker),
			OrdId:     o.Id,
			Price:     e.Price,
			Qty:       o.Qty - o.BrokerExecQty,
		}

		return &fe
	}
	if e.CandleTime.Hour() != o.Ticker.Exchange.MarketOpenTime.Hour {
		return nil
	}

	if e.CandleTime.Minute() != o.Ticker.Exchange.MarketOpenTime.Minute {
		return nil
	}

	fe := OrderFillEvent{
		BaseEvent: be(e.getTime(), e.Ticker),
		OrdId:     o.Id,
		Price:     e.Price,
		Qty:       o.Qty - o.BrokerExecQty,
	}

	return &fe

}

func (b *simBrokerWorker) fillOnCandleOpenLOO(o *simBrokerOrder, e *CandleOpenEvent) event {
	canBeFilled := false
	if e.TimeFrame == "D" || e.TimeFrame == "W" {
		canBeFilled = true

	}

	if !canBeFilled {
		if e.CandleTime.Hour() != o.Ticker.Exchange.MarketOpenTime.Hour {
			return nil
		}

		if e.CandleTime.Minute() != o.Ticker.Exchange.MarketOpenTime.Minute {
			return nil
		}

		canBeFilled = true
	}

	if canBeFilled {
		switch o.Side {
		case OrderBuy:
			if e.Price < o.BrokerPrice || (e.Price == o.BrokerPrice && !b.strictLimitOrders) {
				fe := OrderFillEvent{
					BaseEvent: be(e.getTime(), e.Ticker),
					OrdId:     o.Id,
					Price:     e.Price,
					Qty:       o.Qty - o.BrokerExecQty,
				}

				return &fe
			}
		case OrderSell:
			if e.Price > o.BrokerPrice || (e.Price == o.BrokerPrice && !b.strictLimitOrders) {
				fe := OrderFillEvent{
					BaseEvent: be(e.getTime(), e.Ticker),
					OrdId:     o.Id,
					Price:     e.Price,
					Qty:       o.Qty - o.BrokerExecQty,
				}

				return &fe
			}
		default:
			panic("Unknow side")
		}

		cancelE := OrderCancelEvent{
			BaseEvent: be(e.getTime(), e.Ticker),
			OrdId:     o.Id,
		}

		return &cancelE
	}
	return nil

}

func (b *simBrokerWorker) fillOnCandleOpenMarket(o *simBrokerOrder, e *CandleOpenEvent) event {
	fe := OrderFillEvent{
		BaseEvent: be(e.getTime(), e.Ticker),
		OrdId:     o.Id,
		Price:     e.Price,
		Qty:       o.Qty - o.BrokerExecQty,
	}

	return &fe

}

func (b *simBrokerWorker) fillOnCandleOpenLimit(o *simBrokerOrder, e *CandleOpenEvent) event {
	fillPrice := o.BrokerPrice
	if e.TimeFrame == "D" || e.TimeFrame == "W" {
		fillPrice = e.Price
	}
	switch o.Side {
	case OrderBuy:
		if (e.Price < o.BrokerPrice) || (e.Price == o.BrokerPrice && !b.strictLimitOrders) {
			fe := OrderFillEvent{
				BaseEvent: be(e.getTime(), e.Ticker),
				OrdId:     o.Id,
				Price:     fillPrice,
				Qty:       o.Qty - o.BrokerExecQty,
			}

			return &fe
		}
	case OrderSell:
		if (e.Price > o.BrokerPrice) || (e.Price == o.BrokerPrice && !b.strictLimitOrders) {
			fe := OrderFillEvent{
				BaseEvent: be(e.getTime(), e.Ticker),
				OrdId:     o.Id,
				Price:     fillPrice,
				Qty:       o.Qty - o.BrokerExecQty,
			}

			return &fe
		}
	default:
		panic("Unknown order side: " + string(o.Side))

	}

	if !b.strictLimitOrders && e.Price == o.BrokerPrice {
		fe := OrderFillEvent{
			BaseEvent: be(e.getTime(), e.Ticker),
			OrdId:     o.Id,
			Price:     fillPrice,
			Qty:       o.Qty - o.BrokerExecQty,
		}

		return &fe

	}

	return nil

}

func (b *simBrokerWorker) fillOnCandleOpenStop(o *simBrokerOrder, e *CandleOpenEvent) event {
	switch o.Side {
	case OrderBuy:
		if e.Price > o.BrokerPrice {
			fe := OrderFillEvent{
				BaseEvent: be(e.getTime(), e.Ticker),
				OrdId:     o.Id,
				Price:     e.Price,
				Qty:       o.Qty - o.BrokerExecQty,
			}

			return &fe
		}
	case OrderSell:
		if e.Price < o.BrokerPrice {
			fe := OrderFillEvent{
				BaseEvent: be(e.getTime(), e.Ticker),
				OrdId:     o.Id,
				Price:     e.Price,
				Qty:       o.Qty - o.BrokerExecQty,
			}

			return &fe
		}
	default:
		panic("Unknown order side: " + string(o.Side))

	}

	return nil
}

//********** ON TICK FILLS ***********************************************************************

func (b *simBrokerWorker) fillOnTickLOO(order *simBrokerOrder, tick *Tick) []event {
	err := b.validateOrderForExecution(order, LimitOnOpen)
	if err != nil {
		b.newError(err)
		return nil
	}
	if !tick.IsOpening {
		return nil
	}

	return b.fillOnTickLimitAuction(order, tick)

}

func (b *simBrokerWorker) fillOnTickLOC(order *simBrokerOrder, tick *Tick) []event {
	err := b.validateOrderForExecution(order, LimitOnClose)
	if err != nil {
		b.newError(err)
		return nil
	}

	if !tick.IsClosing {
		return nil
	}

	return b.fillOnTickLimitAuction(order, tick)

}

func (b *simBrokerWorker) fillOnTickLimitAuction(order *simBrokerOrder, tick *Tick) []event {
	var generatedEvents []event
	switch order.Side {
	case OrderSell:
		if tick.LastPrice < order.BrokerPrice {
			cancelE := OrderCancelEvent{
				OrdId:     order.Id,
				BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), tick.Ticker),
			}
			generatedEvents = append(generatedEvents, &cancelE)
			return generatedEvents
		}

	case OrderBuy:
		if tick.LastPrice > order.BrokerPrice {
			cancelE := OrderCancelEvent{
				OrdId:     order.Id,
				BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), tick.Ticker),
			}
			generatedEvents = append(generatedEvents, &cancelE)
			return generatedEvents
		}

	default:
		err := ErrUnknownOrderSide{
			OrdId:   order.Id,
			Message: "From fillOnTickLimitAuction",
			Caller:  "Sim Broker",
		}
		b.newError(&err)
		return nil

	}

	if tick.LastPrice == order.BrokerPrice && b.strictLimitOrders {
		cancelE := OrderCancelEvent{
			OrdId:     order.Id,
			BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), tick.Ticker),
		}
		generatedEvents = append(generatedEvents, &cancelE)
		return generatedEvents
	}

	execQty := order.Qty
	if execQty > tick.LastSize {
		execQty = tick.LastSize
	}

	fillE := OrderFillEvent{
		OrdId:     order.Id,
		Price:     tick.LastPrice,
		Qty:       execQty,
		BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
	}

	generatedEvents = append(generatedEvents, &fillE)

	if execQty < order.Qty {
		cancelE := OrderCancelEvent{
			OrdId:     order.Id,
			BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
		}

		generatedEvents = append(generatedEvents, &cancelE)
	}

	if len(generatedEvents) == 0 {
		return nil
	}
	return generatedEvents

}

func (b *simBrokerWorker) fillOnTickMOO(order *simBrokerOrder, tick *Tick) event {

	if !tick.IsOpening {
		return nil
	}

	if !tick.HasTrade() {
		return nil
	}

	err := b.validateOrderForExecution(order, MarketOnOpen)
	if err != nil {
		b.newError(err)
		return nil
	}

	fillE := OrderFillEvent{
		OrdId:     order.Id,
		Price:     tick.LastPrice,
		Qty:       order.Qty,
		BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
	}
	return &fillE

}

func (b *simBrokerWorker) fillOnTickMOC(order *simBrokerOrder, tick *Tick) event {
	//Todo подумать над реализацией когда отркрывающего тика вообще нет
	if !tick.IsClosing {
		return nil
	}

	if !tick.HasTrade() {
		return nil
	}

	err := b.validateOrderForExecution(order, MarketOnClose)
	if err != nil {
		b.newError(err)
		return nil
	}

	fillE := OrderFillEvent{
		OrdId:     order.Id,
		Price:     tick.LastPrice,
		Qty:       order.Qty,
		BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
	}

	return &fillE

}

func (b *simBrokerWorker) fillOnTickLimit(order *simBrokerOrder, tick *Tick) event {

	if !tick.HasTrade() {
		return nil
	}

	err := b.validateOrderForExecution(order, LimitOrder)
	if err != nil {
		b.newError(err)
		return nil
	}

	lvsQty := order.Qty - order.BrokerExecQty

	switch order.Side {
	case OrderSell:
		if tick.LastPrice > order.BrokerPrice {
			qty := lvsQty
			if tick.LastSize < int64(qty) {
				qty = tick.LastSize
			}

			fillE := OrderFillEvent{
				OrdId:     order.Id,
				Price:     order.BrokerPrice,
				Qty:       qty,
				BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
			}
			return &fillE

		} else {
			if tick.LastPrice == order.BrokerPrice && !b.strictLimitOrders {
				qty := lvsQty
				if tick.LastSize < int64(qty) {
					qty = tick.LastSize
				}

				fillE := OrderFillEvent{
					OrdId:     order.Id,
					Price:     order.BrokerPrice,
					Qty:       qty,
					BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
				}

				return &fillE
			} else {
				return nil
			}
		}

	case OrderBuy:
		if tick.LastPrice < order.BrokerPrice {
			qty := lvsQty
			if tick.LastSize < int64(qty) {
				qty = tick.LastSize
			}

			fillE := OrderFillEvent{
				OrdId:     order.Id,
				Price:     order.BrokerPrice,
				Qty:       qty,
				BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
			}

			return &fillE

		} else {
			if tick.LastPrice == order.BrokerPrice && !b.strictLimitOrders {
				qty := lvsQty
				if tick.LastSize < int64(qty) {
					qty = tick.LastSize
				}

				fillE := OrderFillEvent{
					OrdId:     order.Id,
					Price:     order.BrokerPrice,
					Qty:       qty,
					BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
				}

				return &fillE
			} else {
				return nil
			}
		}
	default:
		b.newError(errors.New("Sim broker: can't check fill for order. Unknown side. "))
		return nil

	}

}

func (b *simBrokerWorker) fillOnTickStop(order *simBrokerOrder, tick *Tick) event {
	if !tick.HasTrade() {
		return nil
	}

	err := b.validateOrderForExecution(order, StopOrder)
	if err != nil {
		b.newError(err)
		return nil
	}

	switch order.Side {
	case OrderSell:
		if tick.LastPrice > order.BrokerPrice {
			return nil
		}
		price := tick.LastPrice
		lvsQty := order.Qty - order.BrokerExecQty
		qty := lvsQty
		if tick.LastSize < qty {
			qty = tick.LastSize
		}
		if tick.HasQuote() {
			price = tick.BidPrice
			qty = lvsQty
		}
		fillE := OrderFillEvent{
			OrdId:     order.Id,
			Price:     price,
			Qty:       qty,
			BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
		}

		return &fillE

	case OrderBuy:
		if tick.LastPrice < order.BrokerPrice {
			return nil
		}
		price := tick.LastPrice
		lvsQty := order.Qty - order.BrokerExecQty
		qty := lvsQty
		if tick.LastSize < qty {
			qty = tick.LastSize
		}
		if tick.HasQuote() {
			price = tick.AskPrice
			qty = lvsQty
		}
		fillE := OrderFillEvent{
			OrdId:     order.Id,
			Price:     price,
			Qty:       qty,
			BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
		}

		return &fillE

	default:
		err := ErrUnknownOrderSide{
			OrdId:   order.Id,
			Message: "Got in fillOnTickStop",
			Caller:  "Sim Broker",
		}
		b.newError(&err)
		return nil
	}

}

func (b *simBrokerWorker) fillOnTickMarket(order *simBrokerOrder, tick *Tick) event {

	if order.Type != MarketOrder {
		b.newError(&ErrUnexpectedOrderType{
			OrdId:        order.Id,
			Caller:       "SimBroker.fillOnTickMarket",
			ExpectedType: string(MarketOrder),
			ActualType:   string(order.Type),
		})

		return nil
	}

	if !order.isValid() {
		err := ErrInvalidOrder{
			OrdId:   order.Id,
			Message: "Can't check executions for invalid order.",
			Caller:  "simBrokerWorker",
		}
		b.newError(&err)
		return nil
	}

	if tick.HasQuote() {
		var qty int64 = 0
		price := math.NaN()
		lvsQty := order.Qty - order.BrokerExecQty

		if order.Side == OrderBuy {
			if lvsQty > tick.AskSize {
				qty = tick.AskSize
			} else {
				qty = lvsQty
			}

			price = tick.AskPrice

		} else { //Short order logic + sanity check for Side issues
			if order.Side != OrderSell {
				b.newError(errors.New("Sim Broker: unknown order side: " + string(order.Side)))
				return nil
			}

			if lvsQty > tick.BidSize {
				qty = tick.BidSize
			} else {
				qty = lvsQty
			}

			price = tick.BidPrice
		}
		fillE := OrderFillEvent{
			OrdId:     order.Id,
			Price:     price,
			Qty:       qty,
			BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
		}

		return &fillE

	} else { // If tick doesn't have quotes. Check on trades
		if !tick.HasTrade() {
			b.newError(errors.New("Sim Broker: tick doesn't contain trade. "))
			return nil
		}

		fillE := OrderFillEvent{
			OrdId:     order.Id,
			Price:     tick.LastPrice,
			Qty:       order.Qty,
			BaseEvent: be(b.genTimeRoundTrip(tick.Datetime), order.Ticker),
		}

		return &fillE
	}

}
