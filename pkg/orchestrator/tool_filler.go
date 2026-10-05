package orchestrator

import (
	"math/rand/v2"
	"sync"
)

// toolFillerPool is what the agent can say while a tool runs and the model wrote nothing of its own
// before the call, so the caller does not sit in silence. One fixed sentence per language ("Let me
// look that up for you.") was spoken on every lookup of every call and read as a recording; a
// handful of short lines, never the same one twice running, reads as a person.
//
// Every line fits any tool (a knowledge-base search, a calendar, a CRM, a web search), so none says
// where it is looking, and none addresses the caller as tu/vous/du/Sie, so one pool serves every
// register. Each is at least three words: the synthesiser cannot condition a one- or two-word
// fragment on the language token, and it comes out sounding English.
func toolFillerPool(lang Language) []string {
	switch lang {
	case LanguageEs:
		return []string{
			"Un momento, lo miro.",
			"Vale, déjame comprobarlo.",
			"Dame un segundo.",
			"Claro, lo busco ahora mismo.",
			"Un segundito, lo compruebo.",
			"A ver, déjame mirar.",
			"Ahora mismo lo miro.",
			"Espera un momento, que lo busco.",
		}
	case LanguageFr:
		return []string{
			"Un instant, je vérifie ça.",
			"Une seconde, je regarde.",
			"D'accord, je cherche ça.",
			"Je regarde ça tout de suite.",
			"Un petit instant, je vérifie.",
			"Je jette un œil, un instant.",
		}
	case LanguageDe:
		return []string{
			"Einen Moment, ich schaue das nach.",
			"Kleinen Moment, ich sehe nach.",
			"Ich schaue kurz nach.",
			"Gleich, ich prüfe das.",
			"Moment, ich suche das raus.",
			"Okay, ich schaue mal nach.",
		}
	case LanguageIt:
		return []string{
			"Un momento, lo controllo.",
			"Un attimo, guardo subito.",
			"Ok, controllo subito.",
			"Un secondo, verifico.",
			"Vedo subito, un attimo.",
			"Ok, vado a vedere.",
		}
	case LanguagePt:
		return []string{
			"Um momento, deixa eu verificar.",
			"Só um segundo, vou ver.",
			"Já vejo isso, um instante.",
			"Deixa eu dar uma olhada.",
			"Um instante, vou confirmar.",
			"Ok, já verifico isso.",
		}
	default:
		return []string{
			"Sure, one moment.",
			"Let me check on that.",
			"Okay, let me take a look.",
			"Give me just a second.",
			"One sec, pulling that up.",
			"Let me see here.",
			"Alright, checking now.",
			"Hang on, let me find that.",
		}
	}
}

// fillerRotation picks from a pool at random, never the line it picked last, so two lookups in a row
// on one call never open with the same words.
type fillerRotation struct {
	mu   sync.Mutex
	last string
	// intn is rand.IntN unless a test pins it.
	intn func(n int) int
}

// next returns an entry of pool other than the previous pick (when the pool has another), "" for an
// empty pool.
func (f *fillerRotation) next(pool []string) string {
	if len(pool) == 0 {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	intn := f.intn
	if intn == nil {
		intn = rand.IntN
	}
	pick := pool[intn(len(pool))]
	if pick == f.last && len(pool) > 1 {
		// Step to the next entry rather than redrawing, so the cost does not depend on luck.
		for i, s := range pool {
			if s == pick {
				pick = pool[(i+1)%len(pool)]
				break
			}
		}
	}
	f.last = pick
	return pick
}

// toolFiller is the line to speak while a tool runs, in the caller's current language.
func (ms *ManagedStream) toolFiller() string {
	return ms.fillers.next(toolFillerPool(ms.session.GetCurrentLanguage()))
}
