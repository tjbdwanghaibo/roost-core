package skill

// Combat result names are a closed vocabulary shared by EventContext and proc filters.
const (
	combatResultHit     = "hit"
	combatResultImmune  = "immune"
	combatResultDodged  = "dodged"
	combatResultBlocked = "blocked"
	combatResultParried = "parried"
	combatResultKilled  = "killed"
)
