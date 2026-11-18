package wallet

import "fmt"

// MaxGold caps a wallet: a credit past it is a business refusal, the case that
// makes the gift saga compensate its debit.
const MaxGold = 1000

// Gold is the wallet's balance.
func (component *Component) Gold() int64 { return component.owner.Dao().GetGold() }

// Spend takes amount out, refusing an overdraft.
func (component *Component) Spend(amount int64) error {
	dao := component.owner.Dao()
	if amount <= 0 || dao.GetGold() < amount {
		return fmt.Errorf("not enough gold: have %d, want %d", dao.GetGold(), amount)
	}
	dao.SetGold(dao.GetGold() - amount)
	return nil
}

// Receive adds amount, refusing to go past MaxGold.
func (component *Component) Receive(amount int64) error {
	dao := component.owner.Dao()
	if amount <= 0 || dao.GetGold()+amount > MaxGold {
		return fmt.Errorf("wallet full: have %d, receive %d, cap %d", dao.GetGold(), amount, MaxGold)
	}
	dao.SetGold(dao.GetGold() + amount)
	return nil
}

// Refund gives amount back without the cap: undoing a debit must not be refused.
func (component *Component) Refund(amount int64) {
	dao := component.owner.Dao()
	dao.SetGold(dao.GetGold() + amount)
}

// LevelUp raises the level by one and returns it.
func (component *Component) LevelUp() int32 {
	dao := component.owner.Dao()
	dao.SetLevel(dao.GetLevel() + 1)
	return dao.GetLevel()
}
