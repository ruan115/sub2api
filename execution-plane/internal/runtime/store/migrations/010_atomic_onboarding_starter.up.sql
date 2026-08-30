ALTER TABLE onboarding_workflows
    ADD UNIQUE KEY uq_onboarding_workflows_intent (intent_id);
