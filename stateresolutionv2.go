/* Copyright 2017 Vector Creations Ltd
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package gomatrixserverlib

import (
	"container/heap"
	"encoding/json"
	"sort"
	"strconv"
)

type stateResolverV2 struct {
	authEventMap              map[string]Event
	powerLevelMainline        []Event
	conflictedPowerLevels     []Event
	conflictedOthers          []Event
	resolvedCreate            *Event
	resolvedPowerLevels       *Event
	resolvedJoinRules         *Event
	resolvedThirdPartyInvites map[string]*Event
	resolvedMembers           map[string]*Event
	result                    []Event
}

func (r *stateResolverV2) Create() (*Event, error) {
	return r.resolvedCreate, nil
}

func (r *stateResolverV2) PowerLevels() (*Event, error) {
	return r.resolvedPowerLevels, nil
}

func (r *stateResolverV2) JoinRules() (*Event, error) {
	return r.resolvedJoinRules, nil
}

func (r *stateResolverV2) ThirdPartyInvite(key string) (*Event, error) {
	return r.resolvedThirdPartyInvites[key], nil
}

func (r *stateResolverV2) Member(key string) (*Event, error) {
	return r.resolvedMembers[key], nil
}

// ResolveStateConflicts takes a list of state events with conflicting state
// keys and works out which event should be used for each state event.
func ResolveStateConflictsV2(conflicted, unconflicted []Event, authEvents []Event) []Event {
	r := stateResolverV2{
		authEventMap:              eventMapFromEvents(authEvents),
		resolvedThirdPartyInvites: make(map[string]*Event),
		resolvedMembers:           make(map[string]*Event),
	}

	// Separate out power level events from the rest of the events. This is
	// necessary because we perform topological ordering of the power events
	// separately, and then the mainline ordering of all other events depends
	// on that power level ordering.
	for _, p := range conflicted {
		if p.Type() == MRoomPowerLevels {
			r.conflictedPowerLevels = append(r.conflictedPowerLevels, p)
		} else {
			r.conflictedOthers = append(r.conflictedOthers, p)
		}
	}

	// Start with the unconflicted events by ordering them topologically and then
	// authing them. The successfully authed events will form the initial partial
	// state.
	unconflicted = r.reverseTopologicalOrdering(unconflicted)
	r.authAndApplyEvents(unconflicted)

	// Then order the conflicted power level events topologically and then also
	// auth those too. The successfully authed events will be layered on top of
	// the partial state.
	r.conflictedPowerLevels = r.reverseTopologicalOrdering(r.conflictedPowerLevels)
	r.authAndApplyEvents(r.conflictedPowerLevels)

	// Then generate the mainline of power level events, order the remaining state
	// events based on the mainline ordering and auth those too. The successfully
	// authed events are also layered on top of the partial state.
	r.powerLevelMainline = r.createPowerLevelMainline()
	r.authAndApplyEvents(r.mainlineOrdering(r.conflictedOthers))

	// Finally we will reapply the original set of unconflicted events onto the //
	// partial state, just in case any of these were overwritten by pulling in //
	// auth events in the previous two steps, and that gives us our final resolved
	// state.
	r.authAndApplyEvents(unconflicted)

	// Now that we have our final state, populate the result array with the
	// resolved state and return it.
	r.result = append(r.result, *r.resolvedCreate)
	r.result = append(r.result, *r.resolvedJoinRules)
	r.result = append(r.result, *r.resolvedPowerLevels)
	for _, member := range r.resolvedMembers {
		r.result = append(r.result, *member)
	}
	for _, invite := range r.resolvedThirdPartyInvites {
		r.result = append(r.result, *invite)
	}
	return r.result
}

// createPowerLevelMainline generates the mainline of power level events,
// starting at the currently resolved power level event from the topological
// ordering and working our way back to the room creation. Note that we populate
// the result here in reverse, so that the room creation is at the beginning of
// the list, rather than the end.
func (r *stateResolverV2) createPowerLevelMainline() []Event {
	var mainline []Event

	// Define our iterator function.
	var iter func(event Event)
	iter = func(event Event) {
		// Append this event to the beginning of the mainline.
		mainline = append([]Event{event}, mainline...)
		// Work through all of the auth event IDs that this event refers to.
		for _, authEventID := range event.AuthEventIDs() {
			// Check that we actually have the auth event in our map - we need this so
			// that we can look up the event type.
			if authEvent, ok := r.authEventMap[authEventID]; ok {
				// Is the event a power event?
				if authEvent.Type() == MRoomPowerLevels {
					// We found a power level event in the event's auth events - start
					// the iterator from this new event.
					iter(authEvent)
				}
			}
		}
	}

	// Begin the sequence from the currently resolved power level event from the
	// topological ordering.
	iter(*r.resolvedPowerLevels)

	return mainline
}

// getFirstPowerLevelMainlineEvent iteratively steps through the auth events of
// the given event until it finds an event that exists in the mainline. Note
// that for this function to work, you must have first called
// createPowerLevelMainline. This function returns three things: the event that
// was found in the mainline, the position in the mainline of the found event
// and the number of steps it took to reach the mainline.
func (r *stateResolverV2) getFirstPowerLevelMainlineEvent(event Event) (
	mainlineEvent Event, mainlinePosition int, steps int,
) {
	// Define a function that the iterator can use to determine whether the event
	// is in the mainline set or not.
	isInMainline := func(searchEvent Event) (bool, int) {
		// Loop through the mainline.
		for pos, mainlineEvent := range r.powerLevelMainline {
			// Check if the search event matches this event. If it does then the event
			// is in the mainline.
			if mainlineEvent.EventID() == searchEvent.EventID() {
				return true, pos
			}
		}
		// If we've reached this point then the event is not in the mainline.
		return false, 0
	}

	// Define our iterator function.
	var iter func(event Event)
	iter = func(event Event) {
		// In much the same way as we do in createPowerLevelMainline, we loop
		// through the event's auth events, checking that it exists in our supplied
		// auth event map and finding power level events.
		for _, authEventID := range event.AuthEventIDs() {
			// Check that we actually have the auth event in our map - we need this so
			// that we can look up the event type.
			if authEvent, ok := r.authEventMap[authEventID]; ok {
				// Is the event a power level event?
				if authEvent.Type() == MRoomPowerLevels {
					// Is the event in the mainline?
					if isIn, pos := isInMainline(authEvent); isIn {
						// It is - take a note of the event and position and stop the
						// iterator from running any further.
						mainlineEvent = authEvent
						mainlinePosition = pos
						return
					}
					// It isn't - increase the step count and then run the iterator again
					// from the found auth event.
					steps++
					iter(authEvent)
				}
			}
		}
	}

	// Start the iterator with the supplied event.
	iter(event)

	return
}

// authAndApplyEvents iterates through the supplied list of events and auths
// them against the current partial state. If they pass the auth checks then we
// also apply them on top of the partial state.
func (r *stateResolverV2) authAndApplyEvents(events []Event) {
	for _, e := range events {
		event := e
		// Check if the event is allowed based on the current partial state. If the
		// event isn't allowed then simply ignore it and process the next one.
		if err := Allowed(event, r); err != nil {
			continue
		}
		// We've now authed the event - work out what the type is and apply it to
		// the partial state based on type.
		switch event.Type() {
		case MRoomCreate:
			// Room creation events are only valid with an empty state key.
			if event.StateKey() == nil || *event.StateKey() == "" {
				r.resolvedCreate = &event
			}
		case MRoomPowerLevels:
			// Power level events are only valid with an empty state key.
			if event.StateKey() == nil || *event.StateKey() == "" {
				r.resolvedPowerLevels = &event
			}
		case MRoomJoinRules:
			// Join rule events are only valid with an empty state key.
			if event.StateKey() == nil || *event.StateKey() == "" {
				r.resolvedJoinRules = &event
			}
		case MRoomThirdPartyInvite:
			// Third party invite events are only valid with a non-empty state key.
			if event.StateKey() != nil && *event.StateKey() != "" {
				r.resolvedThirdPartyInvites[*event.StateKey()] = &event
			}
		case MRoomMember:
			// Membership events are only valid with a non-empty state key.
			if event.StateKey() != nil && *event.StateKey() != "" {
				r.resolvedMembers[*event.StateKey()] = &event
			}
		}
	}
}

// eventMapFromEvents takes a list of events and returns a map, where the key
// for each value is the event ID.
func eventMapFromEvents(events []Event) map[string]Event {
	r := make(map[string]Event)
	for _, e := range events {
		r[e.EventID()] = e
	}
	return r
}

// separate takes a list of events and works out which events are conflicted and
// which are unconflicted.
func separate(events []Event) (conflicted, unconflicted []Event) {
	// The stack maps event type -> event state key -> list of state events.
	stack := make(map[string]map[string][]Event)
	// Prepare the map.
	for _, event := range events {
		// If we haven't encountered an entry of this type yet, create an entry.
		if _, ok := stack[event.Type()]; !ok {
			stack[event.Type()] = make(map[string][]Event)
		}
		// Add the event to the map.
		stack[event.Type()][*event.StateKey()] = append(
			stack[event.Type()][*event.StateKey()], event,
		)
	}
	// Now we need to work out which of these events are conflicted. An event is
	// conflicted if there is more than one entry for the (type, statekey) tuple.
	// If we encounter these events, add them to their relevant conflicted list.
	for _, eventsOfType := range stack {
		for _, eventsOfStateKey := range eventsOfType {
			if len(eventsOfStateKey) > 1 {
				// We have more than one event for the (type, statekey) tuple, therefore
				// these are conflicted.
				for _, event := range eventsOfStateKey {
					conflicted = append(conflicted, event)
				}
			} else if len(eventsOfStateKey) == 1 {
				unconflicted = append(unconflicted, eventsOfStateKey[0])
			}
		}
	}
	return
}

// prepareConflictedEvents takes the input power level events and wraps them in
// stateResV2ConflictedPowerLevel structs so that we have the necessary
// information pre-calculated ahead of sorting.
func (r *stateResolverV2) prepareConflictedEvents(events []Event) []stateResV2ConflictedPowerLevel {
	block := make([]stateResV2ConflictedPowerLevel, len(events))
	for i, event := range events {
		block[i] = stateResV2ConflictedPowerLevel{
			powerLevel:     r.getPowerLevelFromAuthEvents(event),
			originServerTS: int64(event.OriginServerTS()),
			eventID:        event.EventID(),
			event:          event,
		}
	}
	return block
}

// prepareOtherEvents takes the input non-power level events and wraps them in
// stateResV2ConflictedPowerLevel structs so that we have the necessary
// information pre-calculated ahead of sorting.
func (r *stateResolverV2) prepareOtherEvents(events []Event) []stateResV2ConflictedOther {
	block := make([]stateResV2ConflictedOther, len(events))
	for i, event := range events {
		_, pos, _ := r.getFirstPowerLevelMainlineEvent(event)
		block[i] = stateResV2ConflictedOther{
			mainlinePosition: pos,
			originServerTS:   int64(event.OriginServerTS()),
			eventID:          event.EventID(),
			event:            event,
		}
	}
	return block
}

// reverseTopologicalOrdering takes a set of input events, prepares them using
// prepareConflictedEvents and then starts the Kahn's algorithm in order to
// topologically sort them. The result that is returned is correctly ordered.
func (r *stateResolverV2) reverseTopologicalOrdering(events []Event) (result []Event) {
	block := r.prepareConflictedEvents(events)
	sorted := kahnsAlgorithmUsingAuthEvents(block)
	for _, s := range sorted {
		result = append(result, s.event)
	}
	return
}

// mainlineOrdering takes a set of input events, prepares them using
// prepareOtherEvents and then sorts them based on mainline ordering. The result
// that is returned is correctly ordered.
func (r *stateResolverV2) mainlineOrdering(events []Event) (result []Event) {
	block := r.prepareOtherEvents(events)
	sort.Sort(stateResV2ConflictedOtherHeap(block))
	for _, s := range block {
		result = append(result, s.event)
	}
	return
}

// getPowerLevelFromAuthEvents tries to determine the effective power level of
// the sender at the time that of the given event, based on the auth events.
// This is used in the Kahn's algorithm tiebreak.
func (r *stateResolverV2) getPowerLevelFromAuthEvents(event Event) (pl int) {
	for _, authID := range event.AuthEventIDs() {
		// First check and see if we have the auth event in the auth map, if not
		// then we cannot deduce the real effective power level.
		authEvent, ok := r.authEventMap[authID]
		if !ok {
			return 0
		}

		// Ignore the auth event if it isn't a power level event.
		if authEvent.Type() != MRoomPowerLevels || *authEvent.StateKey() != "" {
			continue
		}

		// Try and parse the content of the event.
		var content map[string]interface{}
		if err := json.Unmarshal(authEvent.Content(), &content); err != nil {
			return 0
		}

		// First of all try to see if there's a default user power level. We'll use
		// that for now as a fallback.
		if defaultPl, ok := content["users_default"].(int); ok {
			pl = defaultPl
		}

		// See if there is a "users" key in the event content.
		if users, ok := content["users"].(map[string]string); ok {
			// Is there a key that matches the sender?
			if _, ok := users[event.Sender()]; ok {
				// A power level for this specific user is known, let's use that
				// instead.
				if p, err := strconv.Atoi(users[event.Sender()]); err == nil {
					pl = p
				}
			}
		}
	}

	return
}

// kahnsAlgorithmByAuthEvents is, predictably, an implementation of Kahn's
// algorithm that uses auth events to topologically sort the input list of
// events. This works through each event, counting how many incoming auth event
// dependencies it has, and then adding them into the graph as the dependencies
// are resolved.
func kahnsAlgorithmUsingAuthEvents(events []stateResV2ConflictedPowerLevel) (graph []stateResV2ConflictedPowerLevel) {
	eventMap := make(map[string]stateResV2ConflictedPowerLevel)
	inDegree := make(map[string]int)

	for _, event := range events {
		// For each even that we have been given, add it to the event map so that we
		// can easily refer back to it by event ID later.
		eventMap[event.eventID] = event

		// If we haven't encountered this event ID yet, also start with a zero count
		// of incoming auth event dependencies.
		if _, ok := inDegree[event.eventID]; !ok {
			inDegree[event.eventID] = 0
		}

		// Find each of the auth events that this event depends on and make a note
		// for each auth event that there's an additional incoming dependency.
		for _, auth := range event.event.AuthEventIDs() {
			if _, ok := inDegree[auth]; !ok {
				// We don't know about this event yet - set an initial value.
				inDegree[auth] = 1
			} else {
				// We've already encountered this event so increment instead.
				inDegree[auth]++
			}
		}
	}

	// Now we need to work out which events don't have any incoming auth event
	// dependencies. These will be placed into the graph first. Remove the event
	// from the event map as this prevents us from processing it a second time.
	var noIncoming stateResV2ConflictedPowerLevelHeap
	heap.Init(&noIncoming)
	for eventID, count := range inDegree {
		if count == 0 {
			heap.Push(&noIncoming, eventMap[eventID])
			delete(eventMap, eventID)
		}
	}

	var event stateResV2ConflictedPowerLevel
	for noIncoming.Len() > 0 {
		// Pop the first event ID off the list of events which have no incoming
		// auth event dependencies.
		event = heap.Pop(&noIncoming).(stateResV2ConflictedPowerLevel)

		// Since there are no incoming dependencies to resolve, we can now add this
		// event into the graph.
		graph = append([]stateResV2ConflictedPowerLevel{event}, graph...)

		// Now we should look at the outgoing auth dependencies that this event has.
		// Since this event is now in the graph, the event's outgoing auth
		// dependencies are no longer valid - those map to incoming dependencies on
		// the auth events, so let's update those.
		for _, auth := range event.event.AuthEventIDs() {
			inDegree[auth]--

			// If we see, by updating the incoming dependencies, that the auth event
			// no longer has any incoming dependencies, then it should also be added
			// into the graph on the next pass. In turn, this will also mean that we
			// process the outgoing dependencies of this auth event.
			if inDegree[auth] == 0 {
				if _, ok := eventMap[auth]; ok {
					heap.Push(&noIncoming, eventMap[auth])
					delete(eventMap, auth)
				}
			}
		}
	}

	// The graph is complete at this point!
	return graph
}