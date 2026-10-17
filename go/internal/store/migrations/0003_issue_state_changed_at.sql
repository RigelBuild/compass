-- When SetIssueState last changed an issue's state; NULL until the first
-- transition. The Bridge marks a card that changed column recently as advancing.
ALTER TABLE issues ADD COLUMN state_changed_at TIMESTAMPTZ;
