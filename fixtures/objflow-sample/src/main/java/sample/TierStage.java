package sample;

public class TierStage {
    public void apply(Receipt r, int amount) {
        String t = amount >= 100 ? "GOLD" : "SILVER";
        r.setLabel(t);
    }
}
