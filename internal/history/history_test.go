package history

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nikarpoff/assistant-low-vision-platform/internal/upstream/detector"
)

func TestHistory(t *testing.T) {
	var (
		store *Store
		now   time.Time
		start time.Time
	)

	setup := func(t *testing.T, scenes, turns int) {
		start = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
		now = start
		var err error
		store, err = NewStore(scenes, turns, 600*time.Second, func() time.Time { return now })
		require.NoError(t, err)
	}

	// scene builds a scene with one chair and the description.
	scene := func(description string) Scene {
		return Scene{
			Objects:     []detector.Object{{Label: "chair", Direction: detector.DirectionLeft, Distance: detector.DistanceNear}},
			Description: description,
		}
	}

	// descriptions returns the descriptions of the scenes.
	descriptions := func(history History) []string {
		result := []string{}
		for _, s := range history.Scenes {
			result = append(result, s.Description)
		}
		return result
	}

	// texts returns the texts of the turns.
	texts := func(history History) []string {
		result := []string{}
		for _, turn := range history.Turns {
			result = append(result, turn.Text)
		}
		return result
	}

	t.Run("must_pass_limits", func(t *testing.T) {
		setup(t, 5, 6)

		for i := 1; i <= 7; i++ {
			now = now.Add(time.Second)
			store.AddScene("phone-1", scene(fmt.Sprintf("сцена %d", i)))
		}
		for i := 1; i <= 4; i++ {
			now = now.Add(time.Second)
			store.AddTurn("phone-1",
				Turn{Role: RoleQuestion, Text: fmt.Sprintf("вопрос %d", i)},
				Turn{Role: RoleAnswer, Text: fmt.Sprintf("ответ %d", i)},
			)
		}
		history := store.Get("phone-1")

		require.Equal(t, []string{"сцена 3", "сцена 4", "сцена 5", "сцена 6", "сцена 7"}, descriptions(history))
		require.Equal(t, []string{"вопрос 2", "ответ 2", "вопрос 3", "ответ 3", "вопрос 4", "ответ 4"}, texts(history))
		require.Equal(t, RoleQuestion, history.Turns[0].Role)
		require.Equal(t, RoleAnswer, history.Turns[1].Role)
		last, ok := history.LastScene()
		require.True(t, ok)
		require.Equal(t, "сцена 7", last.Description)
		require.Equal(t, start.Add(7*time.Second), last.At)
		require.Equal(t, start.Add(11*time.Second), history.Turns[5].At)
		received := start.Add(-time.Second)
		store.AddScene("phone-1", Scene{Description: "сцена 8", At: received})
		last, _ = store.Get("phone-1").LastScene()
		require.Equal(t, received, last.At)

		history.Scenes[0].Objects[0].Label = "changed"
		history.Turns[0].Text = "changed"
		again := store.Get("phone-1")
		require.Equal(t, "chair", again.Scenes[0].Objects[0].Label)
		require.Equal(t, "вопрос 2", again.Turns[0].Text)

		setup(t, 0, 0)
		store.AddScene("phone-1", scene("сцена"))
		store.AddTurn("phone-1", Turn{Role: RoleQuestion, Text: "вопрос"})
		require.Empty(t, store.Get("phone-1").Scenes)
		require.Empty(t, store.Get("phone-1").Turns)
		_, ok = store.Get("phone-1").LastScene()
		require.False(t, ok)
	})

	t.Run("must_pass_reconnect_same_device", func(t *testing.T) {
		setup(t, 5, 6)
		store.AddScene("phone-1", scene("до переподключения"))
		store.AddTurn("phone-1", Turn{Role: RoleQuestion, Text: "где дверь"}, Turn{Role: RoleAnswer, Text: "Двери не вижу."})

		now = now.Add(30 * time.Second)
		store.Touch("phone-1")
		history := store.Get("phone-1")

		require.Equal(t, []string{"до переподключения"}, descriptions(history))
		require.Equal(t, []string{"где дверь", "Двери не вижу."}, texts(history))
		now = now.Add(599 * time.Second)
		require.Equal(t, []string{"до переподключения"}, descriptions(store.Get("phone-1")))
	})

	t.Run("must_pass_ttl_expired", func(t *testing.T) {
		setup(t, 5, 6)
		store.AddScene("phone-1", scene("старая сцена"))
		store.AddTurn("phone-1", Turn{Role: RoleQuestion, Text: "старый вопрос"})
		store.AddScene("phone-2", scene("сцена второго"))

		now = now.Add(599 * time.Second)
		beforeTTL := store.Get("phone-1")
		now = now.Add(time.Second)
		afterTTL := store.Get("phone-1")
		store.AddScene("phone-1", scene("новая сцена"))
		renewed := store.Get("phone-1")

		require.Equal(t, []string{"старая сцена"}, descriptions(beforeTTL))
		require.Equal(t, History{}, afterTTL)
		require.Equal(t, []string{"новая сцена"}, descriptions(renewed))
		require.Empty(t, renewed.Turns)
		require.Equal(t, 1, store.Devices())
		require.Equal(t, History{}, store.Get("phone-2"))

		now = now.Add(2 * time.Minute)
		store.Touch("phone-3")
		require.Equal(t, 2, store.Devices())
		now = now.Add(10 * time.Minute)
		store.Touch("phone-3")
		require.Len(t, store.devices, 1)
	})

	t.Run("must_pass_devices_isolated", func(t *testing.T) {
		setup(t, 5, 6)

		store.AddScene("phone-1", scene("сцена первого"))
		store.AddTurn("phone-1", Turn{Role: RoleQuestion, Text: "вопрос первого"})
		store.AddScene("phone-2", scene("сцена второго"))
		store.AddTurn("phone-2", Turn{Role: RoleQuestion, Text: "вопрос второго"})

		require.Equal(t, []string{"сцена первого"}, descriptions(store.Get("phone-1")))
		require.Equal(t, []string{"вопрос первого"}, texts(store.Get("phone-1")))
		require.Equal(t, []string{"сцена второго"}, descriptions(store.Get("phone-2")))
		require.Equal(t, []string{"вопрос второго"}, texts(store.Get("phone-2")))
		require.Equal(t, History{}, store.Get("phone-3"))
	})

	t.Run("must_fail_invalid_settings", func(t *testing.T) {
		setup(t, 5, 6)
		clock := func() time.Time { return now }

		scenes, scenesErr := NewStore(-1, 6, time.Minute, clock)
		turns, turnsErr := NewStore(5, -1, time.Minute, clock)
		ttl, ttlErr := NewStore(5, 6, 0, clock)

		require.Nil(t, scenes)
		require.EqualError(t, scenesErr, "invalid APP_HISTORY_SCENES: a non-negative number is required: value=-1")
		require.Nil(t, turns)
		require.EqualError(t, turnsErr, "invalid APP_HISTORY_TURNS: a non-negative number is required: value=-1")
		require.Nil(t, ttl)
		require.EqualError(t, ttlErr, "invalid APP_HISTORY_TTL_S: a positive duration is required: value=0s")
	})
}
