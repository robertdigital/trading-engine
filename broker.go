package engine

import (
	"alex/marketdata"
	"errors"
	"time"
	"math"
)

type IBroker interface {
	Connect(errChan chan error, eventChan chan *event) error
	OnNewOrder(e *NewOrderEvent)
	OnCancelRequest(e *OrderCancelRequestEvent)
	IsSimulated() bool
	OnCandleClose(candle *marketdata.Candle)
	OnCandleOpen(price float64)
	OnTick(tick *marketdata.Tick)
	NextEvent()
	PopEvent()
}

type SimulatedBroker struct {
	errChan              chan error
	eventChan            chan *event
	filledOrders         map[string]*Order
	canceledOrders       map[string]*Order
	confirmedOrders      map[string]*Order
	rejectedOrders       map[string]*Order
	allOrders            map[string]*Order
	delay                int64
	hasQuotesAndTrades   bool
	strictLimitOrders    bool
	marketOpenUntilTime  TimeOfDay
	marketCloseUntilTime TimeOfDay
}

func (b *SimulatedBroker) IsSimulated() bool {
	return true
}

func (b *SimulatedBroker) Connect(errChan chan error, eventChan chan *event) error {
	if errChan == nil {
		return errors.New("Can't connect simulated broker. Error chan is nil. ")
	}
	if eventChan == nil {
		return errors.New("Can't connect simulated broker. Event chan is nil. ")
	}
	b.errChan = errChan
	b.eventChan = eventChan

	b.filledOrders = make(map[string]*Order)
	b.canceledOrders = make(map[string]*Order)
	b.confirmedOrders = make(map[string]*Order)
	b.rejectedOrders = make(map[string]*Order)
	b.allOrders = make(map[string]*Order)

	return nil
}

func (b *SimulatedBroker) OnNewOrder(e *NewOrderEvent) {

	if !e.LinkedOrder.isValid() {
		r := "Sim Broker: can't confirm order. Order is not valid"
		rejectEvent := OrderRejectedEvent{OrdId: e.LinkedOrder.Id, Reason: r, Time: time.Now()}
		go b.newEvent(&rejectEvent)
		b.rejectedOrders[e.LinkedOrder.Id] = e.LinkedOrder
		b.allOrders[e.LinkedOrder.Id] = e.LinkedOrder
		return
	}

	if _, ok := b.allOrders[e.LinkedOrder.Id]; ok {
		r := "Sim Broker: can't confirm order. Order with this ID already exists on broker side"
		rejectEvent := OrderRejectedEvent{OrdId: e.LinkedOrder.Id, Reason: r, Time: time.Now()}
		go b.newEvent(&rejectEvent)
		b.rejectedOrders[e.LinkedOrder.Id] = e.LinkedOrder

		return
	}

	b.allOrders[e.LinkedOrder.Id] = e.LinkedOrder
	confEvent := OrderConfirmationEvent{e.LinkedOrder.Id, time.Now()}

	go b.newEvent(&confEvent)

	b.confirmedOrders[e.LinkedOrder.Id] = e.LinkedOrder
}

func (b *SimulatedBroker) OnCancelRequest(e *OrderCancelRequestEvent) {
	if _, ok := b.confirmedOrders[e.OrdId]; !ok {
		go b.newError(errors.New("Sim broker: Can't cancel order. ID not found in confirmed. "))
		return
	}
	if b.confirmedOrders[e.OrdId].State != ConfirmedOrder {
		go b.newError(errors.New("Sim broker: Can't cancel order. Order state is not ConfirmedOrder "))
		return
	}
	b.canceledOrders[e.OrdId] = b.confirmedOrders[e.OrdId]
	delete(b.confirmedOrders, e.OrdId)
	orderCancelE := OrderCancelEvent{OrdId: e.OrdId, Time: e.Time.Add(time.Duration(b.delay) * time.Millisecond)}

	go b.newEvent(&orderCancelE)

}

func (b *SimulatedBroker) OnTick(tick *marketdata.Tick) {
	if !b.tickIsValid(tick) {
		err := ErrBrokenTick{
			Tick:    *tick,
			Message: "Got in OnTick",
			Caller:  "Sim Broker",
		}

		go b.newError(&err)
		return

	}
	if len(b.confirmedOrders) == 0 {
		return
	}

	for _, o := range b.confirmedOrders {
		b.checkOrderExecutionOnTick(o, tick)
	}

}

func (b *SimulatedBroker) tickIsValid(tick *marketdata.Tick) bool {
	if tick.HasQuote {
		if math.IsNaN(tick.BidPrice) || math.IsNaN(tick.AskPrice) || tick.BidPrice == 0 || tick.AskPrice == 0 {
			return false
		}
	}

	if tick.HasTrade {
		if math.IsNaN(tick.LastPrice) || tick.LastPrice == 0 || tick.LastSize == 0 {
			return false
		}
	}
	return true
}

func (b *SimulatedBroker) checkOrderExecutionOnTick(order *Order, tick *marketdata.Tick) {

	switch order.Type {
	case MarketOrder:
		b.checkOnTickMarket(order, tick)
		return
	case LimitOrder:
		b.checkOnTickLimit(order, tick)
		return
	case StopOrder:
		b.checkOnTickStop(order, tick)
		return
	case LimitOnClose:
		b.checkOnTickLOC(order, tick)
		return
	case LimitOnOpen:
		b.checkOnTickLOO(order, tick)
		return
	case MarketOnOpen:
		b.checkOnTickMOO(order, tick)
		return
	case MarketOnClose:
		b.checkOnTickMOC(order, tick)
		return
	default:
		err := ErrUnknownOrderType{
			OrdId:   order.Id,
			Message: "found order with type: " + string(order.Type),
			Caller:  "Sim Broker",
		}
		go b.newError(&err)
	}

}

func (b *SimulatedBroker) checkOnTickLOO(order *Order, tick *marketdata.Tick) {
	err := b.validateOrderForExecution(order, LimitOnOpen)
	if err != nil {
		go b.newError(err)
		return
	}

	if !tick.IsOpening {
		if b.marketOpenUntilTime.Before(tick.Datetime) {
			b.updateCanceledOrders(order)
			cancelE := OrderCancelEvent{
				OrdId: order.Id,
				Time:  tick.Datetime,
			}
			go b.newEvent(&cancelE)
		}
		return
	}

	b.checkOnTickLimitAuction(order, tick)

}

func (b *SimulatedBroker) checkOnTickLOC(order *Order, tick *marketdata.Tick) {
	err := b.validateOrderForExecution(order, LimitOnClose)
	if err != nil {
		go b.newError(err)
		return
	}

	if !tick.IsClosing {
		if b.marketCloseUntilTime.Before(tick.Datetime) {
			b.updateCanceledOrders(order)
			cancelE := OrderCancelEvent{
				OrdId: order.Id,
				Time:  tick.Datetime,
			}
			go b.newEvent(&cancelE)
		}
		return
	}

	b.checkOnTickLimitAuction(order, tick)

}

func (b *SimulatedBroker) checkOnTickLimitAuction(order *Order, tick *marketdata.Tick) {
	switch order.Side {
	case OrderSell:
		if tick.LastPrice < order.Price {
			cancelE := OrderCancelEvent{
				OrdId: order.Id,
				Time:  tick.Datetime,
			}
			b.updateCanceledOrders(order)
			go b.newEvent(&cancelE)
			return
		}

	case OrderBuy:
		if tick.LastPrice > order.Price {
			cancelE := OrderCancelEvent{
				OrdId: order.Id,
				Time:  tick.Datetime,
			}
			b.updateCanceledOrders(order)
			go b.newEvent(&cancelE)
			return
		}

	default:
		err := ErrUnknownOrderSide{
			OrdId:   order.Id,
			Message: "From checkOnTickLimitAuction",
			Caller:  "Sim Broker",
		}
		go b.newError(&err)
		return

	}

	if tick.LastPrice == order.Price && b.strictLimitOrders {
		cancelE := OrderCancelEvent{
			OrdId: order.Id,
			Time:  tick.Datetime,
		}
		b.updateCanceledOrders(order)
		go b.newEvent(&cancelE)
		return
	}

	execQty := order.Qty
	if execQty > int(tick.LastSize) {
		execQty = int(tick.LastSize)
	}

	fillE := OrderFillEvent{
		OrdId:  order.Id,
		Symbol: order.Symbol,
		Price:  tick.LastPrice,
		Qty:    execQty,
		Time:   tick.Datetime,
	}

	b.updateFilledOrders(order, execQty)
	go b.newEvent(&fillE)

	if execQty < order.Qty {
		b.updateCanceledOrders(order)
		cancelE := OrderCancelEvent{
			OrdId: order.Id,
			Time:  tick.Datetime,
		}
		go b.newEvent(&cancelE)
	}

}

func (b *SimulatedBroker) checkOnTickMOO(order *Order, tick *marketdata.Tick) {
	err := b.validateOrderForExecution(order, MarketOnOpen)
	if err != nil {
		go b.newError(err)
		return
	}

	if !tick.IsOpening {
		return
	}

	if !tick.HasTrade {
		return
	}

	fillE := OrderFillEvent{
		OrdId:  order.Id,
		Symbol: order.Symbol,
		Price:  tick.LastPrice,
		Qty:    order.Qty,
		Time:   tick.Datetime,
	}
	b.updateFilledOrders(order, order.Qty)
	go b.newEvent(&fillE)
	return

}

func (b *SimulatedBroker) checkOnTickMOC(order *Order, tick *marketdata.Tick) {
	//Todo подумать над реализацией когда отркрывающего тика вообще нет
	err := b.validateOrderForExecution(order, MarketOnClose)
	if err != nil {
		go b.newError(err)
		return
	}

	if !tick.IsClosing {
		return
	}

	if !tick.HasTrade {
		return
	}

	fillE := OrderFillEvent{
		OrdId:  order.Id,
		Symbol: order.Symbol,
		Price:  tick.LastPrice,
		Qty:    order.Qty,
		Time:   tick.Datetime,
	}
	b.updateFilledOrders(order, order.Qty)
	go b.newEvent(&fillE)
	return

}

func (b *SimulatedBroker) checkOnTickLimit(order *Order, tick *marketdata.Tick) {

	err := b.validateOrderForExecution(order, LimitOrder)
	if err != nil {
		go b.newError(err)
		return
	}

	if !tick.HasTrade {
		return
	}

	if math.IsNaN(tick.LastPrice) {
		return
	}
	lvsQty := order.Qty - order.ExecQty
	if lvsQty <= 0 {
		go b.newError(errors.New("Sim broker: Lvs qty is zero or less. Nothing to execute. "))
		return
	}
	switch order.Side {
	case OrderSell:
		if tick.LastPrice > order.Price {
			qty := lvsQty
			if tick.LastSize < int64(qty) {
				qty = int(tick.LastSize)
			}

			fillE := OrderFillEvent{
				OrdId:  order.Id,
				Symbol: order.Symbol,
				Price:  order.Price,
				Qty:    qty,
				Time:   tick.Datetime,
			}

			b.updateFilledOrders(order, qty)
			go b.newEvent(&fillE)
			return

		} else {
			if tick.LastPrice == order.Price && !b.strictLimitOrders {
				qty := lvsQty
				if tick.LastSize < int64(qty) {
					qty = int(tick.LastSize)
				}

				fillE := OrderFillEvent{
					OrdId:  order.Id,
					Symbol: order.Symbol,
					Price:  order.Price,
					Qty:    qty,
					Time:   tick.Datetime,
				}

				b.updateFilledOrders(order, qty)
				go b.newEvent(&fillE)
				return
			} else {
				return
			}
		}

	case OrderBuy:
		if tick.LastPrice < order.Price {
			qty := lvsQty
			if tick.LastSize < int64(qty) {
				qty = int(tick.LastSize)
			}

			fillE := OrderFillEvent{
				OrdId:  order.Id,
				Symbol: order.Symbol,
				Price:  order.Price,
				Qty:    qty,
				Time:   tick.Datetime,
			}

			b.updateFilledOrders(order, qty)

			go b.newEvent(&fillE)
			return

		} else {
			if tick.LastPrice == order.Price && !b.strictLimitOrders {
				qty := lvsQty
				if tick.LastSize < int64(qty) {
					qty = int(tick.LastSize)
				}

				fillE := OrderFillEvent{
					OrdId:  order.Id,
					Symbol: order.Symbol,
					Price:  order.Price,
					Qty:    qty,
					Time:   tick.Datetime,
				}
				b.updateFilledOrders(order, qty)
				go b.newEvent(&fillE)
				return
			} else {
				return
			}
		}
	default:
		go b.newError(errors.New("Sim broker: can't check fill for order. Unknown side. "))
		return

	}

}

func (b *SimulatedBroker) checkOnTickStop(order *Order, tick *marketdata.Tick) {
	err := b.validateOrderForExecution(order, StopOrder)
	if err != nil {
		go b.newError(err)
		return
	}

	if !tick.HasTrade {
		return
	}

	switch order.Side {
	case OrderSell:
		if tick.LastPrice > order.Price {
			return
		}
		price := tick.LastPrice
		lvsQty := order.Qty - order.ExecQty
		qty := lvsQty
		if int(tick.LastSize) < qty {
			qty = int(tick.LastSize)
		}
		if tick.HasQuote {
			price = tick.BidPrice
			qty = lvsQty
		}
		fillE := OrderFillEvent{
			OrdId:  order.Id,
			Symbol: order.Symbol,
			Price:  price,
			Qty:    qty,
			Time:   tick.Datetime,
		}

		b.updateFilledOrders(order, qty)
		go b.newEvent(&fillE)
		return

	case OrderBuy:
		if tick.LastPrice < order.Price {
			return
		}
		price := tick.LastPrice
		lvsQty := order.Qty - order.ExecQty
		qty := lvsQty
		if int(tick.LastSize) < qty {
			qty = int(tick.LastSize)
		}
		if tick.HasQuote {
			price = tick.AskPrice
			qty = lvsQty
		}
		fillE := OrderFillEvent{
			OrdId:  order.Id,
			Symbol: order.Symbol,
			Price:  price,
			Qty:    qty,
			Time:   tick.Datetime,
		}

		b.updateFilledOrders(order, qty)
		go b.newEvent(&fillE)
		return

	default:
		err := ErrUnknownOrderSide{
			OrdId:   order.Id,
			Message: "Got in checkOnTickStop",
			Caller:  "Sim Broker",
		}
		go b.newError(&err)
		return
	}

}

func (b *SimulatedBroker) checkOnTickMarket(order *Order, tick *marketdata.Tick) {
	err := b.validateOrderForExecution(order, MarketOrder)
	if err != nil {
		go b.newError(err)
		return
	}

	if b.hasQuotesAndTrades && !tick.HasQuote {
		return
	}
	if !b.hasQuotesAndTrades && tick.HasQuote {
		go b.newError(errors.New("Sim Broker: broker doesn't expect quotes. Only trades. "))
		return
	}

	if b.hasQuotesAndTrades {
		qty := 0
		price := math.NaN()
		lvsQty := order.Qty - order.ExecQty

		if order.Side == OrderBuy {
			if int64(lvsQty) > tick.AskSize { //Todo Smell
				qty = int(tick.AskSize)
			} else {
				qty = lvsQty
			}

			price = tick.AskPrice

		} else { //Short order logic + sanity check for Side issues
			if order.Side != OrderSell {
				go b.newError(errors.New("Sim Broker: unknown order side: " + string(order.Side)))
				return
			}

			if int64(lvsQty) > tick.BidSize { //Todo Smell
				qty = int(tick.BidSize)
			} else {
				qty = lvsQty
			}

			price = tick.BidPrice
		}
		fillE := OrderFillEvent{
			OrdId:  order.Id,
			Symbol: order.Symbol,
			Price:  price,
			Qty:    qty,
			Time:   tick.Datetime,
		}
		b.updateFilledOrders(order, qty)

		go b.newEvent(&fillE)

	} else { //If broker accepts only trades without quotes
		if !tick.HasTrade {
			go b.newError(errors.New("Sim Broker: tick doesn't contain trade. "))
			return
		}

		fillE := OrderFillEvent{
			OrdId:  order.Id,
			Symbol: order.Symbol,
			Price:  tick.LastPrice,
			Qty:    order.Qty,
			Time:   tick.Datetime,
		}

		b.updateFilledOrders(order, order.Qty)
		go b.newEvent(&fillE)
	}

}

//validateOrderForExecution checks if order is valid and can be filled. Returns nil if order is valid
//or error in other cases
func (b *SimulatedBroker) validateOrderForExecution(order *Order, expectedType OrderType) error {
	if !order.isValid() {
		err := ErrInvalidOrder{
			OrdId:   order.Id,
			Message: "Got in checkOnTickLimit",
			Caller:  "Sim Broker",
		}

		return &err
	}

	if order.Type != expectedType {
		err := ErrUnexpectedOrderType{
			OrdId:        order.Id,
			ActualType:   string(order.Type),
			ExpectedType: string(expectedType),
			Message:      "Got in checkOnTickLimit",
			Caller:       "Sim Broker",
		}
		return &err
	}

	if order.State != ConfirmedOrder && order.State != PartialFilledOrder {
		err := ErrUnexpectedOrderState{
			OrdId:         order.Id,
			ActualState:   string(order.State),
			ExpectedState: string(ConfirmedOrder) + "," + string(PartialFilledOrder),
			Message:       "Got in checkOnTickLimit",
			Caller:        "Sim Broker",
		}
		return &err
	}

	return nil
}

func (b *SimulatedBroker) updateFilledOrders(order *Order, execQty int) {
	if execQty == order.Qty-order.ExecQty {
		b.filledOrders[order.Id] = order
		delete(b.confirmedOrders, order.Id)
	}
}

func (b *SimulatedBroker) updateCanceledOrders(order *Order) {
	b.canceledOrders[order.Id] = order
	delete(b.confirmedOrders, order.Id)
}

func (b *SimulatedBroker) newEvent(e event) {
	if b.eventChan == nil {
		panic("Simulated broker event chan is nil")
	}
	time.Sleep(time.Duration(b.delay) * time.Millisecond)
	b.eventChan <- &e

}

func (b *SimulatedBroker) newError(e error) {
	if b.errChan == nil {
		panic("Simulated broker error chan is nil")
	}
	b.errChan <- e
}
