-- 0065: the room check happens INSIDE the insert, not before it.
--
-- 0064 added chat_rooms and a Go-side check in QueueChatPush: read
-- whether the destination is a known room, then enqueue. A review of
-- #290 pointed out that those are two operations, and the gap between
-- them is reachable:
--
--     automation                     inbound adapter
--     ----------                     ---------------
--     IsChatRoom(target) -> false
--                                    NoteChatRoom(target)  [commits]
--     EnqueueChatOutbound(push)      <- lands in a room
--
-- The window is small and the consequence is the one thing this whole
-- slice exists to prevent: household or personal content delivered to
-- a room. "Small window" is not a property a disclosure boundary gets
-- to rely on, and a household rule fires on a schedule that nobody is
-- watching.
--
-- A TRIGGER RATHER THAN A TRANSACTION IN THE CALLER, for the reason
-- ADR-W033's ceilings are decorators: a rule enforced at one call site
-- is a rule the next call site has to remember. This one holds for
-- every writer of chat_outbound, including a future one nobody has
-- written yet, and it holds by the shape of the schema rather than by
-- everybody wrapping their insert correctly.
--
-- PUSHES ONLY, and the distinction is the same one EnqueueChatOutbound
-- already draws for delivery quarantine. A PUSH is the house deciding
-- to say something: a rule's question, a notification, content
-- assembled from what the household knows. A REPLY answers somebody
-- who just spoke, and the one reply a room ever receives is the
-- redirect that tells a person to message the Agent directly (design
-- §8.3) -- which carries no name, no fact, and not even what was
-- asked. Blocking that too would leave a household member talking to
-- a bot that has gone silent.

CREATE TRIGGER chat_outbound_no_push_into_a_room
BEFORE INSERT ON chat_outbound
WHEN NEW.kind = 'push'
 AND EXISTS (
     SELECT 1 FROM chat_rooms r
      WHERE r.connection = NEW.connection AND r.conversation = NEW.target)
BEGIN
    SELECT RAISE(ABORT,
        'that destination is a room, and the house does not send what it knows into a room (ADR-W033 B9.1, design 8.3)');
END;
